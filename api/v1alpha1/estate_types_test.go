package v1alpha1

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The spec `tq` renders round-trips through the Go types byte for byte: an explicit
// false or an empty capability list survives, an omitted pin or backup stays omitted, the
// interval stays "15m".
func TestRenderedSpecRoundTrips(t *testing.T) {
	for _, name := range []string{"rendered-spec-dev.json", "rendered-spec-prod.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.TrimSpace(raw)
		var spec EstateSpec
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, raw) {
			t.Errorf("%s does not round-trip:\n in  %s\n out %s", name, raw, out)
		}
	}
}
