package supplychain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSelfVerifyAtRungObserveNeverFails(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{"empty.json": "[]", "two.json": `[{"kid":"a"},{"kid":"b"}]`, "doc.json": `{"keys":[{"kid":"a"}]}`, "bad.json": "{"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for file, keys := range map[string]int{"empty.json": 0, "two.json": 2, "doc.json": 1, "bad.json": 0, "absent.json": 0} {
		r := SelfVerify("registry.tequila.dev/platform/tq-operator:v0.1.0", filepath.Join(dir, file))
		if r.Signature != Unverified || !r.OK() || r.Keys != keys {
			t.Errorf("%s: %+v", file, r)
		}
	}
	if (Result{Signature: Invalid}).OK() {
		t.Error("an invalid signature is not OK")
	}
}
