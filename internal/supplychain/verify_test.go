package supplychain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

var stampedAt = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

type fixture struct {
	reg     *testRegistry
	image   string // by digest
	tag     string // the same image by tag
	signer  *keyPair
	other   *keyPair
	tsa     *testTSA
	foreign *testTSA
}

type keyPair struct {
	priv *ecdsa.PrivateKey
	pem  string
	kid  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	reg := newTestRegistry(t)
	digest := reg.pushImage(t, "v1.0.0")
	signerPriv, signerPEM := testKeyPair(t)
	otherPriv, otherPEM := testKeyPair(t)
	return &fixture{
		reg: reg, image: digest.String(), tag: reg.repo.Tag("v1.0.0").String(),
		signer:  &keyPair{priv: signerPriv, pem: signerPEM, kid: Kid(&signerPriv.PublicKey)},
		other:   &keyPair{priv: otherPriv, pem: otherPEM, kid: Kid(&otherPriv.PublicKey)},
		tsa:     newTestTSA(t, stampedAt),
		foreign: newTestTSA(t, stampedAt),
	}
}

func (f *fixture) verifier(t *testing.T, keys []byte) *Verifier {
	t.Helper()
	parsed, err := ParseKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	return &Verifier{Keys: parsed, Transport: f.reg.tr, Now: func() time.Time { return stampedAt.Add(time.Hour) }}
}

func mustECDSA(_ *testing.T, k *keyPair) *ecdsa.PrivateKey { return k.priv }

func TestVerifyValidSignatureWithTimestamp(t *testing.T) {
	f := newFixture(t)
	digest := f.reg.repo.Digest(strings.SplitN(f.image, "@", 2)[1])
	f.reg.pushSignatures(t, digest, sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), f.tsa.stamp(t, stampedAt)))
	v := f.verifier(t, keysJSON(keyEntryJSON(f.other.kid, f.other.pem, "", nil), keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, nil)))

	for name, ref := range map[string]string{"by digest": f.image, "by tag": f.tag} {
		r := v.Verify(context.Background(), ref)
		if r.Signature != Valid || r.Kid != f.signer.kid || !r.Timestamp.Equal(stampedAt) || r.Reason != ReasonVerified || r.Keys != 2 || !r.OK() {
			t.Errorf("%s: %+v", name, r)
		}
		if !strings.Contains(r.Message, f.signer.kid) || !strings.Contains(r.Message, "2026-10-12T12:00:00Z") {
			t.Errorf("%s: message %q", name, r.Message)
		}
		st := r.Status()
		if st.Signature != "valid" || st.Kid == nil || *st.Kid != f.signer.kid || st.Timestamp == nil || !st.Timestamp.Time.Equal(stampedAt) || st.CheckedAt.Time.IsZero() || st.Reason != "verified" {
			t.Errorf("%s: status %+v", name, st)
		}
	}
	// The release's signer is tried first; the outcome is the same either way.
	v.SignerKid = f.signer.kid
	if r := v.Verify(context.Background(), f.image); r.Signature != Valid || r.Kid != f.signer.kid {
		t.Errorf("signer first: %+v", r)
	}
}

func TestVerifyWrongKeyIsInvalid(t *testing.T) {
	f := newFixture(t)
	digest := f.reg.repo.Digest(strings.SplitN(f.image, "@", 2)[1])
	f.reg.pushSignatures(t, digest, sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), f.tsa.stamp(t, stampedAt)))
	v := f.verifier(t, keysJSON(keyEntryJSON(f.other.kid, f.other.pem, f.tsa.chainPEM, nil)))
	r := v.Verify(context.Background(), f.image)
	if r.Signature != Invalid || r.Reason != ReasonNoMatchingKey || r.OK() || r.Kid != "" {
		t.Errorf("%+v", r)
	}
	if st := r.Status(); st.Kid != nil || st.Timestamp != nil || st.Signature != "invalid" {
		t.Errorf("status %+v", st)
	}
}

func TestVerifyRevocationIsDecidedByTheTimestamp(t *testing.T) {
	f := newFixture(t)
	digest := f.reg.repo.Digest(strings.SplitN(f.image, "@", 2)[1])
	before, after := stampedAt.Add(-time.Hour), stampedAt.Add(time.Hour)

	f.reg.pushSignatures(t, digest, sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), f.tsa.stamp(t, stampedAt)))
	if r := f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, &before))).Verify(context.Background(), f.image); r.Signature != Invalid || r.Reason != ReasonKeyRevoked {
		t.Errorf("revoked before the timestamp: %+v", r)
	}
	if r := f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, &after))).Verify(context.Background(), f.image); r.Signature != Valid || r.Reason != ReasonVerified {
		t.Errorf("revoked after the timestamp: %+v", r)
	}

	// Without a timestamp nothing proves the signature predates the revocation.
	f.reg.pushSignatures(t, digest, sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), nil))
	if r := f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, &after))).Verify(context.Background(), f.image); r.Signature != Invalid || r.Reason != ReasonKeyRevoked {
		t.Errorf("revoked, no timestamp: %+v", r)
	}
	if r := f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, nil))).Verify(context.Background(), f.image); r.Signature != Valid || !r.Timestamp.IsZero() || !strings.Contains(r.Message, "no timestamp") {
		t.Errorf("not revoked, no timestamp: %+v", r)
	}
}

func TestVerifyTimestampMustChainToTheKeysAuthorities(t *testing.T) {
	f := newFixture(t)
	digest := f.reg.repo.Digest(strings.SplitN(f.image, "@", 2)[1])
	f.reg.pushSignatures(t, digest, sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), f.foreign.stamp(t, stampedAt)))
	if r := f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, nil))).Verify(context.Background(), f.image); r.Signature != Invalid || r.Reason != ReasonTimestampInvalid {
		t.Errorf("a timestamp by another authority: %+v", r)
	}
	if r := f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, "", nil))).Verify(context.Background(), f.image); r.Signature != Invalid || r.Reason != ReasonTimestampInvalid {
		t.Errorf("a timestamp with no chain to check it against: %+v", r)
	}
	// A timestamp over something other than this signature.
	s := sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), nil)
	s.annotations[TimestampAnnotation] = f.tsa.stamp(t, stampedAt)([]byte("another signature"))
	f.reg.pushSignatures(t, digest, s)
	if r := f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, nil))).Verify(context.Background(), f.image); r.Signature != Invalid || r.Reason != ReasonTimestampInvalid || !strings.Contains(r.Message, "something other") {
		t.Errorf("a timestamp over other bytes: %+v", r)
	}
}

func TestVerifyPayloadMustNameTheImagesDigest(t *testing.T) {
	f := newFixture(t)
	digest := f.reg.repo.Digest(strings.SplitN(f.image, "@", 2)[1])
	otherImage := f.reg.pushImage(t, "v0.9.0")
	// A valid signature — over another image — copied beside this one.
	f.reg.pushSignatures(t, digest, sign(t, f.reg.repo, otherImage.DigestStr(), mustECDSA(t, f.signer), f.tsa.stamp(t, stampedAt)))
	if r := f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, nil))).Verify(context.Background(), f.image); r.Signature != Invalid || r.Reason != ReasonPayloadMismatch {
		t.Errorf("%+v", r)
	}
}

func TestVerifyUnsignedAndMalformed(t *testing.T) {
	f := newFixture(t)
	digest := f.reg.repo.Digest(strings.SplitN(f.image, "@", 2)[1])
	keys := keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, nil))
	if r := f.verifier(t, keys).Verify(context.Background(), f.image); r.Signature != Unsigned || r.Reason != ReasonUnsigned || !r.OK() || !strings.Contains(r.Message, ".sig") {
		t.Errorf("no signature tag: %+v", r)
	}
	f.reg.pushSignatures(t, digest) // the tag exists, with no layer
	if r := f.verifier(t, keys).Verify(context.Background(), f.image); r.Signature != Unsigned || r.Reason != ReasonUnsigned {
		t.Errorf("an empty signature manifest: %+v", r)
	}
	s := sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), nil)
	s.annotations[SignatureAnnotation] = "not base64!"
	f.reg.pushSignatures(t, digest, s)
	if r := f.verifier(t, keys).Verify(context.Background(), f.image); r.Signature != Invalid || r.Reason != ReasonMalformedSignature {
		t.Errorf("a signature that is not base64: %+v", r)
	}
	// Two layers: one bad, one good — a good one anywhere is valid.
	good := sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), f.tsa.stamp(t, stampedAt))
	f.reg.pushSignatures(t, digest, s, good)
	if r := f.verifier(t, keys).Verify(context.Background(), f.image); r.Signature != Valid {
		t.Errorf("a good layer after a bad one: %+v", r)
	}
}

func TestVerifyWithoutKeysOrImageTouchesNoRegistry(t *testing.T) {
	f := newFixture(t)
	before := f.reg.requestCount()
	v := &Verifier{Transport: f.reg.tr}
	if r := v.Verify(context.Background(), f.image); r.Signature != Unverified || r.Reason != ReasonNoKeys || !r.OK() {
		t.Errorf("no keys: %+v", r)
	}
	v = f.verifier(t, keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, "", nil)))
	if r := v.Verify(context.Background(), ""); r.Signature != Unverified || r.Reason != ReasonNoImage {
		t.Errorf("no image: %+v", r)
	}
	if r := v.Verify(context.Background(), "not a reference!"); r.Signature != Unverified || r.Reason != ReasonBadImageReference {
		t.Errorf("a bad reference: %+v", r)
	}
	if n := f.reg.requestCount() - before; n != 0 {
		t.Errorf("%d registry requests before any verification could start", n)
	}
	if r := v.Verify(context.Background(), f.reg.repo.Tag("v9.9.9").String()); r.Signature != Unverified || r.Reason != ReasonImageNotFound {
		t.Errorf("a tag the registry does not have: %+v", r)
	}
}

func TestVerifyRegistryAuthentication(t *testing.T) {
	f := newFixture(t)
	digest := f.reg.repo.Digest(strings.SplitN(f.image, "@", 2)[1])
	f.reg.pushSignatures(t, digest, sign(t, f.reg.repo, digest.DigestStr(), mustECDSA(t, f.signer), f.tsa.stamp(t, stampedAt)))
	keys := keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, nil))
	f.reg.requirePassword("token-for-the-registry")

	// Anonymous: the registry challenges and nobody answers.
	if r := f.verifier(t, keys).Verify(context.Background(), f.image); r.Signature != Unverified || r.Reason != ReasonRegistryAuth || !strings.Contains(r.Message, "401") {
		t.Errorf("anonymous: %+v", r)
	}
	// No token to be had (IAM refused the registry's audience): unverified, registry-auth, and
	// the detail reaches the message while the credential never would.
	v := f.verifier(t, keys)
	v.Credential = func(context.Context) (string, string, error) {
		return "", "IAM answered 400 invalid_target — the estate is not granted that audience", errors.New("invalid_target")
	}
	if r := v.Verify(context.Background(), f.image); r.Signature != Unverified || r.Reason != ReasonRegistryAuth || !strings.Contains(r.Message, "invalid_target") {
		t.Errorf("no credential: %+v", r)
	}
	// A credential the realm refuses.
	v.Credential = func(context.Context) (string, string, error) { return "wrong", "", nil }
	if r := v.Verify(context.Background(), f.image); r.Signature != Unverified || r.Reason != ReasonRegistryAuth || strings.Contains(r.Message, "wrong") {
		t.Errorf("a refused credential: %+v", r)
	}
	// The right one: the realm's bearer opens the repository.
	v.Credential = func(context.Context) (string, string, error) { return "token-for-the-registry", "", nil }
	if r := v.Verify(context.Background(), f.image); r.Signature != Valid || r.Kid != f.signer.kid {
		t.Errorf("the right credential: %+v", r)
	}
}

func TestVerifyUnreachableRegistry(t *testing.T) {
	f := newFixture(t)
	keys := keysJSON(keyEntryJSON(f.signer.kid, f.signer.pem, f.tsa.chainPEM, nil))
	v := f.verifier(t, keys)
	v.Transport = roundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp: connection refused") })
	if r := v.Verify(context.Background(), f.image); r.Signature != Unverified || r.Reason != ReasonRegistryUnreachable || !r.OK() {
		t.Errorf("%+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v.Transport = f.reg.tr
	if r := v.Verify(ctx, f.image); r.Signature != Unverified || r.Reason != ReasonRegistryUnreachable {
		t.Errorf("a cancelled context: %+v", r)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
