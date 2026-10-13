package supplychain

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// maxKeysFileBytes caps the signing-keys file.
const maxKeysFileBytes = 1 << 20

// Key is one entry of the rendered signing-key list: a public key, its lifetime, and the
// timestamp-authority chains a timestamp by this key's signatures is verified against.
type Key struct {
	Kid       string
	Algorithm string
	ValidFrom time.Time
	RetiredAt *time.Time
	RevokedAt *time.Time
	Public    *ecdsa.PublicKey
	TSA       TSATrust
}

// TSATrust is the trust material of a key's timestamp authorities: the self-signed roots and
// every other certificate of the chains.
type TSATrust struct {
	Roots         *x509.CertPool
	Intermediates *x509.CertPool
	Certificates  int
}

// Keys is the rendered signing-key list, in the file's order.
type Keys []Key

// keyEntry is the wire shape of one key — the catalogue's signing-key list verbatim.
type keyEntry struct {
	Kid          string  `json:"kid"`
	PublicKeyPem string  `json:"publicKeyPem"`
	Algorithm    string  `json:"algorithm"`
	ValidFrom    string  `json:"validFrom"`
	RetiredAt    *string `json:"retiredAt"`
	RevokedAt    *string `json:"revokedAt"`
	TSAChains    []struct {
		Name string `json:"name"`
		Pem  string `json:"pem"`
	} `json:"tsaChains"`
}

// LoadKeys reads the signing-keys file: a bare list of keys or `{"keys": [...]}` (both are the
// catalogue's shape), or the empty list a render carries before signing ships. An absent file
// is no keys and no error — the ConfigMap is optional on the Pod. A file that is not one of
// those shapes, or a key whose PEM is not an ECDSA P-256 public key, is an error.
func LoadKeys(path string) (Keys, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the signing-keys file: %w", err)
	}
	if len(raw) > maxKeysFileBytes {
		return nil, fmt.Errorf("the signing-keys file is larger than %d bytes", maxKeysFileBytes)
	}
	return ParseKeys(raw)
}

// ParseKeys parses the signing-keys document (see LoadKeys).
func ParseKeys(raw []byte) (Keys, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var entries []keyEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		var doc struct {
			Keys []keyEntry `json:"keys"`
		}
		if err2 := json.Unmarshal(raw, &doc); err2 != nil {
			return nil, errors.New("the signing-keys file is neither a list of keys nor {\"keys\": [...]}")
		}
		entries = doc.Keys
	}
	keys := make(Keys, 0, len(entries))
	for i, e := range entries {
		k, err := parseKey(e)
		if err != nil {
			return nil, fmt.Errorf("signing key %d (%s): %w", i, orNoKid(e.Kid), err)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func parseKey(e keyEntry) (Key, error) {
	pub, err := parsePublicKey(e.PublicKeyPem)
	if err != nil {
		return Key{}, err
	}
	kid := e.Kid
	if kid == "" {
		kid = Kid(pub)
	}
	k := Key{Kid: kid, Algorithm: e.Algorithm, Public: pub}
	if e.ValidFrom != "" {
		if k.ValidFrom, err = time.Parse(time.RFC3339, e.ValidFrom); err != nil {
			return Key{}, fmt.Errorf("validFrom %q is not RFC 3339", e.ValidFrom)
		}
	}
	if k.RetiredAt, err = optionalTime("retiredAt", e.RetiredAt); err != nil {
		return Key{}, err
	}
	if k.RevokedAt, err = optionalTime("revokedAt", e.RevokedAt); err != nil {
		return Key{}, err
	}
	for _, chain := range e.TSAChains {
		if err := k.TSA.add(chain.Pem); err != nil {
			return Key{}, fmt.Errorf("tsaChains %q: %w", chain.Name, err)
		}
	}
	return k, nil
}

func parsePublicKey(pemText string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("publicKeyPem is not a PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("publicKeyPem: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("publicKeyPem is not an ECDSA P-256 key")
	}
	return pub, nil
}

// Kid is a key's identifier: the first 16 hex characters of SHA-256 over the key's SPKI DER —
// derivable from the PEM by anyone.
func Kid(pub *ecdsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:16]
}

func optionalTime(field string, v *string) (*time.Time, error) {
	if v == nil || *v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *v)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not RFC 3339", field, *v)
	}
	return &t, nil
}

// add files every certificate of one PEM chain: a self-signed CA is a root, everything else an
// intermediate (a leaf in the chain is harmless there — the token's own signer is what the
// chain is built from).
func (t *TSATrust) add(pemText string) error {
	rest := []byte(pemText)
	n := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("certificate %d: %w", n, err)
		}
		if t.Roots == nil {
			t.Roots, t.Intermediates = x509.NewCertPool(), x509.NewCertPool()
		}
		if cert.IsCA && isSelfSigned(cert) {
			t.Roots.AddCert(cert)
		} else {
			t.Intermediates.AddCert(cert)
		}
		t.Certificates++
		n++
	}
	if n == 0 {
		return errors.New("no CERTIFICATE block")
	}
	return nil
}

func isSelfSigned(cert *x509.Certificate) bool {
	return cert.Subject.String() == cert.Issuer.String() && cert.CheckSignatureFrom(cert) == nil
}

// Empty reports whether the trust holds no certificate.
func (t TSATrust) Empty() bool { return t.Certificates == 0 }

func orNoKid(kid string) string {
	if kid == "" {
		return "no kid"
	}
	return kid
}
