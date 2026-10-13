package supplychain

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testKeyPair(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return priv, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "\n", `\n`) + `"`
}

func TestParseKeysBothShapesAndTheEmptyList(t *testing.T) {
	priv, pubPEM := testKeyPair(t)
	entry := `{"kid":"abc","publicKeyPem":` + jsonString(pubPEM) + `,"algorithm":"ecdsa-p256-sha256","validFrom":"2026-10-01T00:00:00Z","retiredAt":null,"revokedAt":"2026-12-01T00:00:00Z","tsaChains":[]}`
	for name, tc := range map[string]struct {
		raw  string
		keys int
	}{
		"bare list":        {"[" + entry + "]", 1},
		"keys document":    {`{"keys":[` + entry + "," + entry + "]}", 2},
		"empty list":       {"[]", 0},
		"empty document":   {`{"keys":[]}`, 0},
		"blank file":       {"  \n", 0},
		"no kid: derived":  {`[{"publicKeyPem":` + jsonString(pubPEM) + `}]`, 1},
		"no times, no tsa": {`[{"kid":"k","publicKeyPem":` + jsonString(pubPEM) + `}]`, 1},
	} {
		keys, err := ParseKeys([]byte(tc.raw))
		if err != nil || len(keys) != tc.keys {
			t.Errorf("%s: %d keys, %v", name, len(keys), err)
		}
	}
	keys, err := ParseKeys([]byte("[" + entry + "]"))
	if err != nil {
		t.Fatal(err)
	}
	k := keys[0]
	if k.Kid != "abc" || k.Algorithm != "ecdsa-p256-sha256" || !k.Public.Equal(&priv.PublicKey) {
		t.Errorf("key = %+v", k)
	}
	if k.ValidFrom != time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) || k.RetiredAt != nil || k.RevokedAt == nil || !k.RevokedAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("times = %v %v %v", k.ValidFrom, k.RetiredAt, k.RevokedAt)
	}
	if !k.TSA.Empty() {
		t.Error("no tsaChains yet a chain")
	}
	derived, _ := ParseKeys([]byte(`[{"publicKeyPem":` + jsonString(pubPEM) + `}]`))
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	sum := sha256.Sum256(der)
	if want := hex.EncodeToString(sum[:])[:16]; derived[0].Kid != want || Kid(&priv.PublicKey) != want || len(want) != 16 {
		t.Errorf("kid = %q, want %q (sha256 of the SPKI DER, first 16 hex characters)", derived[0].Kid, want)
	}
}

func TestParseKeysRefusesWhatIsNotAKeyList(t *testing.T) {
	_, pubPEM := testKeyPair(t)
	rsaPEM := "-----BEGIN PUBLIC KEY-----\nMFwwDQYJKoZIhvcNAQEBBQADSwAwSAJBAKyXDF/1RQ/2U3YNAk/UQhCwDdsqkEM3\nLBx4GEGvqeOq8bpfgikVC1rngwaCbtl7WqDTiXAiWlRf8tdYCg4Bre8CAwEAAQ==\n-----END PUBLIC KEY-----\n"
	for name, raw := range map[string]string{
		"not json":      "{",
		"a string":      `"[]"`,
		"bad pem":       `[{"kid":"a","publicKeyPem":"nope"}]`,
		"not ecdsa":     `[{"kid":"a","publicKeyPem":` + jsonString(rsaPEM) + `}]`,
		"bad validFrom": `[{"kid":"a","publicKeyPem":` + jsonString(pubPEM) + `,"validFrom":"yesterday"}]`,
		"bad revokedAt": `[{"kid":"a","publicKeyPem":` + jsonString(pubPEM) + `,"revokedAt":"soon"}]`,
		"bad tsa chain": `[{"kid":"a","publicKeyPem":` + jsonString(pubPEM) + `,"tsaChains":[{"name":"x","pem":"not a certificate"}]}]`,
	} {
		if _, err := ParseKeys([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadKeysAbsentFileIsNoKeys(t *testing.T) {
	keys, err := LoadKeys(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || keys != nil {
		t.Errorf("absent: %v, %v", keys, err)
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if keys, err := LoadKeys(path); err != nil || len(keys) != 0 {
		t.Errorf("empty: %v, %v", keys, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeys(path); err == nil {
		t.Error("a malformed file is an error, not an empty list")
	}
}

func TestTSATrustSplitsRootsFromTheRest(t *testing.T) {
	tsa := newTestTSA(t, time.Now())
	var trust TSATrust
	if err := trust.add(tsa.chainPEM); err != nil {
		t.Fatal(err)
	}
	if trust.Certificates != 2 || trust.Empty() {
		t.Errorf("trust = %+v", trust)
	}
	if err := trust.add("-----BEGIN PUBLIC KEY-----\nAA==\n-----END PUBLIC KEY-----\n"); err == nil {
		t.Error("a chain without a CERTIFICATE block is an error")
	}
}
