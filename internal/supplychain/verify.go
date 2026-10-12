package supplychain

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/digitorus/pkcs7"
	"github.com/digitorus/timestamp"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The signature layout cosign 2.x writes beside an image.
const (
	// SignatureTagSuffix: the manifest tagged sha256-<digest>.sig holds the image's signatures.
	SignatureTagSuffix = ".sig"
	// SimpleSigningMediaType is the media type of a signature layer: its blob is the payload.
	SimpleSigningMediaType types.MediaType = "application/vnd.dev.cosign.simplesigning.v1+json"
	// SignatureAnnotation carries the base64 ECDSA signature over SHA-256 of the payload.
	SignatureAnnotation = "dev.cosignproject.cosign/signature"
	// TimestampAnnotation carries {"SignedRFC3161Timestamp": <base64 DER TimeStampResp>}, the
	// timestamp authority's token over the raw signature bytes.
	TimestampAnnotation = "dev.sigstore.cosign/rfc3161timestamp"
	// PayloadType is the simple-signing payload's critical.type.
	PayloadType = "cosign container image signature"
	// RegistryUser is the Basic username the registry's realm sees: a label — the identity is
	// the password, a token of the registry's audience.
	RegistryUser = "tq-operator"

	// maxPayloadBytes caps one signature payload; maxSignatureLayers caps how many are read.
	maxPayloadBytes    = 64 * 1024
	maxSignatureLayers = 16
)

// Credential returns the registry's password — an IAM token of the registry's audience — or
// why none could be had (a detail for the condition, never the token itself).
type Credential func(ctx context.Context) (token string, detail string, err error)

// Verifier reads an image's signature from its registry and verifies it with the rendered keys.
type Verifier struct {
	Keys Keys
	// SignerKid, when set, is the key the catalogue names as this release's signer: tried
	// first; every other key after.
	SignerKid string
	// Credential is how the verifier authenticates at the registry; nil means anonymously.
	Credential Credential
	// Transport is the registry client's transport (report.NewTransport in the operator).
	Transport http.RoundTripper
	UserAgent string
	Now       func() time.Time
}

// Verify checks image's signature. Every outcome is a Result; nothing here returns an error
// to the caller, because the operator continues whatever the verdict is.
func (v *Verifier) Verify(ctx context.Context, image string) Result {
	now := v.now()
	keys := len(v.Keys)
	if keys == 0 {
		return unverified(image, ReasonNoKeys, "no signing keys are rendered (the signing-keys ConfigMap is empty); verification arrives with the catalogue's keys", 0, now)
	}
	if strings.TrimSpace(image) == "" {
		return unverified(image, ReasonNoImage, "the operator's image reference is not set (TQ_OPERATOR_IMAGE / --image)", keys, now)
	}
	ref, err := name.ParseReference(image, name.StrictValidation)
	if err != nil {
		return unverified(image, ReasonBadImageReference, err.Error(), keys, now)
	}
	opts := []remote.Option{remote.WithContext(ctx)}
	if v.Transport != nil {
		opts = append(opts, remote.WithTransport(v.Transport))
	}
	if v.UserAgent != "" {
		opts = append(opts, remote.WithUserAgent(v.UserAgent))
	}
	if v.Credential != nil {
		token, detail, err := v.Credential(ctx)
		if err != nil {
			return unverified(image, ReasonRegistryAuth, "no credential for the registry: "+detail, keys, now)
		}
		opts = append(opts, remote.WithAuth(&authn.Basic{Username: RegistryUser, Password: token}))
	}

	digest, err := v.digestOf(ref, opts)
	if err != nil {
		return unverified(image, registryReason(err, ReasonImageNotFound), "resolving the image's digest: "+registryDetail(err), keys, now)
	}
	sigRef := ref.Context().Tag(strings.Replace(digest.String(), ":", "-", 1) + SignatureTagSuffix)
	desc, err := remote.Get(sigRef, opts...)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			return Result{Signature: Unsigned, CheckedAt: now, Reason: ReasonUnsigned, Keys: keys,
				Message: fmt.Sprintf("%s is unsigned: the registry holds no %s for %s", image, sigRef.TagStr(), digest)}
		}
		return unverified(image, registryReason(err, ReasonRegistryUnreachable), "reading the signature: "+registryDetail(err), keys, now)
	}
	sigImage, err := desc.Image()
	if err != nil {
		return invalid(image, ReasonMalformedSignature, "the signature manifest: "+err.Error(), keys, now)
	}
	manifest, err := sigImage.Manifest()
	if err != nil {
		return invalid(image, ReasonMalformedSignature, "the signature manifest: "+err.Error(), keys, now)
	}

	var first *Result
	read := 0
	for _, layer := range manifest.Layers {
		if layer.MediaType != SimpleSigningMediaType {
			continue
		}
		if read == maxSignatureLayers {
			break
		}
		read++
		payload, err := readPayload(sigImage, layer.Digest)
		if err != nil {
			return unverified(image, registryReason(err, ReasonRegistryUnreachable), "reading the signature payload: "+registryDetail(err), keys, now)
		}
		r := v.verifyLayer(image, digest, payload, layer.Annotations, keys, now)
		if r.Signature == Valid {
			return r
		}
		if first == nil {
			first = &r
		}
	}
	if first == nil {
		return Result{Signature: Unsigned, CheckedAt: now, Reason: ReasonUnsigned, Keys: keys,
			Message: fmt.Sprintf("%s is unsigned: %s carries no signature layer", image, sigRef.TagStr())}
	}
	return *first
}

// digestOf resolves the reference to the digest the signature is over: the reference's own
// when it is pinned by digest, else what the registry serves for the tag now.
func (v *Verifier) digestOf(ref name.Reference, opts []remote.Option) (v1.Hash, error) {
	if d, ok := ref.(name.Digest); ok {
		return v1.NewHash(d.DigestStr())
	}
	desc, err := remote.Head(ref, opts...)
	if err != nil {
		return v1.Hash{}, err
	}
	return desc.Digest, nil
}

func readPayload(img v1.Image, digest v1.Hash) ([]byte, error) {
	layer, err := img.LayerByDigest(digest)
	if err != nil {
		return nil, err
	}
	rc, err := layer.Compressed()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	payload, err := io.ReadAll(io.LimitReader(rc, maxPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxPayloadBytes {
		return nil, fmt.Errorf("the signature payload is larger than %d bytes", maxPayloadBytes)
	}
	return payload, nil
}

// simpleSigning is the part of the simple-signing payload the verifier reads.
type simpleSigning struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

// verifyLayer verifies one signature: the ECDSA signature over the payload by one of the keys
// (the release's signer first), the payload's digest against the image's, then the timestamp
// against the key's lifetime.
func (v *Verifier) verifyLayer(image string, digest v1.Hash, payload []byte, annotations map[string]string, keys int, now time.Time) Result {
	sig, err := base64.StdEncoding.DecodeString(annotations[SignatureAnnotation])
	if err != nil || len(sig) == 0 {
		return invalid(image, ReasonMalformedSignature, "the signature annotation is not base64", keys, now)
	}
	sum := sha256.Sum256(payload)
	key, ok := v.signer(sum[:], sig)
	if !ok {
		return invalid(image, ReasonNoMatchingKey, fmt.Sprintf("the signature verifies with none of the %d rendered keys", keys), keys, now)
	}

	var p simpleSigning
	if err := json.Unmarshal(payload, &p); err != nil || p.Critical.Type != PayloadType {
		return invalid(image, ReasonMalformedSignature, "the signed payload is not a simple-signing payload", keys, now)
	}
	if p.Critical.Image.DockerManifestDigest != digest.String() {
		return invalid(image, ReasonPayloadMismatch, fmt.Sprintf("the signature by key %s is over %s, not %s", key.Kid, p.Critical.Image.DockerManifestDigest, digest), keys, now)
	}

	var ts time.Time
	if raw, has := annotations[TimestampAnnotation]; has && raw != "" {
		ts, err = verifyTimestamp(key, sig, raw)
		if err != nil {
			return invalid(image, ReasonTimestampInvalid, fmt.Sprintf("the timestamp on the signature by key %s: %s", key.Kid, err), keys, now)
		}
	}
	if key.RevokedAt != nil {
		switch {
		case ts.IsZero():
			return invalid(image, ReasonKeyRevoked, fmt.Sprintf("key %s was revoked at %s and the signature carries no timestamp that proves it predates the revocation", key.Kid, key.RevokedAt.UTC().Format(time.RFC3339)), keys, now)
		case !ts.Before(*key.RevokedAt):
			return invalid(image, ReasonKeyRevoked, fmt.Sprintf("the signature was timestamped %s, after key %s was revoked at %s", ts.UTC().Format(time.RFC3339), key.Kid, key.RevokedAt.UTC().Format(time.RFC3339)), keys, now)
		}
	}
	if !ts.IsZero() && !key.ValidFrom.IsZero() && ts.Before(key.ValidFrom) {
		return invalid(image, ReasonKeyNotYetValid, fmt.Sprintf("the signature was timestamped %s, before key %s is valid from %s", ts.UTC().Format(time.RFC3339), key.Kid, key.ValidFrom.UTC().Format(time.RFC3339)), keys, now)
	}
	msg := fmt.Sprintf("%s verifies with key %s", image, key.Kid)
	if ts.IsZero() {
		msg += " (no timestamp)"
	} else {
		msg += " (timestamped " + ts.UTC().Format(time.RFC3339) + ")"
	}
	return Result{Signature: Valid, Kid: key.Kid, Timestamp: ts, CheckedAt: now, Reason: ReasonVerified, Message: msg, Keys: keys}
}

// signer finds the key the signature verifies with: the release's signer first, then every
// key in the file's order. Revoked and retired keys are tried too — whether a signature by a
// revoked key counts is the timestamp's question, not the key's.
func (v *Verifier) signer(digest, sig []byte) (Key, bool) {
	if v.SignerKid != "" {
		for _, k := range v.Keys {
			if k.Kid == v.SignerKid && ecdsa.VerifyASN1(k.Public, digest, sig) {
				return k, true
			}
		}
	}
	for _, k := range v.Keys {
		if k.Kid != v.SignerKid && ecdsa.VerifyASN1(k.Public, digest, sig) {
			return k, true
		}
	}
	return Key{}, false
}

// verifyTimestamp checks the RFC 3161 token in the annotation: it is over SHA-256 of the raw
// signature, its signer chains to one of the key's timestamp-authority roots with the
// time-stamping extended key usage, valid at the time it attests. Returns that time.
func verifyTimestamp(key Key, sig []byte, annotation string) (time.Time, error) {
	var env struct {
		SignedRFC3161Timestamp []byte `json:"SignedRFC3161Timestamp"`
	}
	if err := json.Unmarshal([]byte(annotation), &env); err != nil || len(env.SignedRFC3161Timestamp) == 0 {
		return time.Time{}, errors.New("the timestamp annotation is not {\"SignedRFC3161Timestamp\": <base64>}")
	}
	if key.TSA.Empty() {
		return time.Time{}, fmt.Errorf("key %s names no timestamp-authority chain to verify it against", key.Kid)
	}
	ts, err := timestamp.ParseResponse(env.SignedRFC3161Timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("not an RFC 3161 response: %w", err)
	}
	if ts.HashAlgorithm != crypto.SHA256 {
		return time.Time{}, errors.New("the timestamp's message imprint is not SHA-256")
	}
	want := sha256.Sum256(sig)
	if subtle.ConstantTimeCompare(ts.HashedMessage, want[:]) != 1 {
		return time.Time{}, errors.New("the timestamp is over something other than this signature")
	}
	token, err := pkcs7.Parse(ts.RawToken)
	if err != nil {
		return time.Time{}, fmt.Errorf("the timestamp token: %w", err)
	}
	intermediates := key.TSA.Intermediates.Clone()
	for _, cert := range token.Certificates {
		intermediates.AddCert(cert)
	}
	if err := token.VerifyWithOpts(x509.VerifyOptions{
		Roots:         key.TSA.Roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
		CurrentTime:   ts.Time,
	}); err != nil {
		return time.Time{}, fmt.Errorf("the timestamp's signer does not chain to a trusted timestamp authority: %w", err)
	}
	return ts.Time, nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// statusOf is the HTTP status behind a registry error, 0 when it was not an HTTP answer.
func statusOf(err error) int {
	var te *transport.Error
	if errors.As(err, &te) {
		return te.StatusCode
	}
	return 0
}

// registryReason classifies a registry error: a 401/403 is the operator's credential, a 404
// is notFound, anything else is the registry not answering as a registry should.
func registryReason(err error, notFound string) string {
	switch statusOf(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ReasonRegistryAuth
	case http.StatusNotFound:
		return notFound
	default:
		return ReasonRegistryUnreachable
	}
}

// registryDetail is a registry error fit for a condition: the status and the registry's own
// error codes, never a URL with a credential in it.
func registryDetail(err error) string {
	var te *transport.Error
	if errors.As(err, &te) {
		var codes []string
		for _, d := range te.Errors {
			codes = append(codes, string(d.Code))
		}
		s := fmt.Sprintf("the registry answered %d", te.StatusCode)
		if len(codes) > 0 {
			s += " " + strings.Join(codes, ",")
		}
		return s
	}
	var buf bytes.Buffer
	buf.WriteString(err.Error())
	if buf.Len() > 200 {
		buf.Truncate(200)
		buf.WriteString("…")
	}
	return buf.String()
}
