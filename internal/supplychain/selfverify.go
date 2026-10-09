// Package supplychain verifies the operator's own image.
//
// At rung observe this is a stub: the platform's signing keys reach the render with the
// release catalogue, which does not exist yet, so the operator reads its signing-keys file —
// an empty list for now — and reports its own signature as unverified. cosign's Go packages arrive with the keys,
// and with them a SelfVerificationFailed Ready condition when the check fails.
package supplychain

import (
	"encoding/json"
	"fmt"
	"os"
)

// Signature is the outcome of verifying an image (the report's image signature values).
type Signature string

const (
	// Unverified: no verification was attempted.
	Unverified Signature = "unverified"
	// Valid, Invalid and Unsigned are rung verify's.
	Valid    Signature = "valid"
	Invalid  Signature = "invalid"
	Unsigned Signature = "unsigned"
)

// Result is the self-check's outcome.
type Result struct {
	Signature Signature
	// Keys is how many public keys the signing-keys file holds.
	Keys    int
	Message string
}

// OK reports whether the result is not a failed verification.
func (r Result) OK() bool { return r.Signature != Invalid }

// SelfVerify checks the operator's own image against the rendered signing keys. At rung observe
// it never fails: an absent or empty key list is the expected state until signing ships.
func SelfVerify(image, keysFile string) Result {
	keys := 0
	if raw, err := os.ReadFile(keysFile); err == nil {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil {
			keys = len(list)
		} else {
			var doc struct {
				Keys []json.RawMessage `json:"keys"`
			}
			if json.Unmarshal(raw, &doc) == nil {
				keys = len(doc.Keys)
			}
		}
	}
	return Result{
		Signature: Unverified,
		Keys:      keys,
		Message:   fmt.Sprintf("rung observe: %s is not verified (%d signing keys rendered; verification arrives with the catalogue)", orUnknown(image), keys),
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "the operator's image"
	}
	return s
}
