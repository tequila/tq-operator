// Package supplychain verifies the operator's own image.
//
// The operator's render carries the platform's public signing keys (a ConfigMap rendered from
// the catalogue's signing-key list) and the operator's own image reference. At start and then
// once per interval the operator reads the image's signature from the registry the kubelet
// pulled it from, verifies it with those keys, and writes the verdict into
// Estate.status.selfVerification and the report. A failed verdict is an Observed: False
// condition with reason SelfVerificationFailed — and continued operation: the operator never
// stops observing because of its own supply chain.
//
// The signature format is the one `cosign sign --key … --tlog-upload=false
// --timestamp-server-url …` writes (cosign 2.x, the legacy tag layout): a manifest tagged
// `sha256-<digest>.sig` beside the image, one layer per signature carrying the simple-signing
// payload, the ECDSA P-256 signature over SHA-256 of that payload in the layer's
// `dev.cosignproject.cosign/signature` annotation and, when timestamped, an RFC 3161 token over
// the raw signature bytes in `dev.sigstore.cosign/rfc3161timestamp`. The verifier here is the
// minimal reading of that format on the standard library plus a registry client and an RFC
// 3161 parser — no transparency log, no certificate identity, no key fetched at run time.
package supplychain

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
)

// Signature is the outcome of verifying an image (the status' and the report's signature values).
type Signature string

const (
	// Unverified: no verdict was reached.
	Unverified Signature = "unverified"
	// Valid: a signature by one of the rendered keys, timestamped before the key's revocation
	// when it is timestamped at all.
	Valid Signature = "valid"
	// Invalid: a signature that does not verify, or one by a key revoked before its timestamp.
	Invalid Signature = "invalid"
	// Unsigned: the registry holds no signature for the image's digest.
	Unsigned Signature = "unsigned"
)

// The one-word reasons of Estate.status.selfVerification.reason.
const (
	ReasonVerified            = "verified"
	ReasonNotChecked          = "not-checked"
	ReasonNoKeys              = "no-keys"
	ReasonKeysMalformed       = "keys-malformed"
	ReasonNoImage             = "no-image"
	ReasonBadImageReference   = "bad-image-reference"
	ReasonImageNotFound       = "image-not-found"
	ReasonRegistryAuth        = "registry-auth"
	ReasonRegistryUnreachable = "registry-unreachable"
	ReasonUnsigned            = "unsigned"
	ReasonNoMatchingKey       = "no-matching-key"
	ReasonPayloadMismatch     = "payload-mismatch"
	ReasonKeyRevoked          = "key-revoked"
	ReasonKeyNotYetValid      = "key-not-yet-valid"
	ReasonTimestampInvalid    = "timestamp-invalid"
	ReasonMalformedSignature  = "malformed-signature"
)

// Result is the self-check's outcome.
type Result struct {
	Signature Signature
	// Kid is the key the signature verified with (Valid only).
	Kid string
	// Timestamp is the verified RFC 3161 time the signature carries; zero when none.
	Timestamp time.Time
	CheckedAt time.Time
	// Reason is one of the Reason* words.
	Reason string
	// Message is the condition's text — prose for a human, never a credential.
	Message string
	// Keys is how many signing keys were available to the check.
	Keys int
}

// OK reports whether the result is not a failed verification: unverified and unsigned are
// not failures — they are the states before signing reaches a render.
func (r Result) OK() bool { return r.Signature != Invalid }

// Status renders the result as the Estate's status block.
func (r Result) Status() estatev1alpha1.SelfVerification {
	out := estatev1alpha1.SelfVerification{
		Signature: string(r.Signature),
		CheckedAt: metav1.NewTime(r.CheckedAt.UTC().Truncate(time.Second)),
		Reason:    r.Reason,
	}
	if r.Kid != "" {
		kid := r.Kid
		out.Kid = &kid
	}
	if !r.Timestamp.IsZero() {
		ts := metav1.NewTime(r.Timestamp.UTC().Truncate(time.Second))
		out.Timestamp = &ts
	}
	return out
}

// NotChecked is the result before the first check completed.
func NotChecked(image string, at time.Time) Result {
	return Result{
		Signature: Unverified,
		CheckedAt: at,
		Reason:    ReasonNotChecked,
		Message:   fmt.Sprintf("%s is not verified yet: the first check has not completed", orUnknown(image)),
	}
}

func unverified(image, reason, detail string, keys int, at time.Time) Result {
	return Result{
		Signature: Unverified, CheckedAt: at, Reason: reason, Keys: keys,
		Message: fmt.Sprintf("%s is not verified (%s): %s", orUnknown(image), reason, detail),
	}
}

func invalid(image, reason, detail string, keys int, at time.Time) Result {
	return Result{
		Signature: Invalid, CheckedAt: at, Reason: reason, Keys: keys,
		Message: fmt.Sprintf("%s does not verify (%s): %s", orUnknown(image), reason, detail),
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "the operator's image"
	}
	return s
}
