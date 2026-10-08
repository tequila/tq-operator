package schemagen

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := ModuleRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func generated(t *testing.T) (report, estate map[string]any, reportRaw, estateRaw []byte) {
	t.Helper()
	reportRaw, estateRaw, err := Generate(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reportRaw, &report); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(estateRaw, &estate); err != nil {
		t.Fatal(err)
	}
	return report, estate, reportRaw, estateRaw
}

// The committed schemas are what the types generate — `make manifests` was run.
func TestCommittedSchemasAreFresh(t *testing.T) {
	_, _, reportRaw, estateRaw := generated(t)
	root := moduleRoot(t)
	for path, want := range map[string][]byte{ReportSchemaPath: reportRaw, EstateSchemaPath: estateRaw} {
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale — run `make manifests`", path)
		}
	}
}

// The generated report schema equals the hand-written contract, path for path.
func TestReportSchemaEqualsTheContract(t *testing.T) {
	report, _, _, _ := generated(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "report.shape"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			want = append(want, line)
		}
	}
	got := Shape(report)
	if !slices.Equal(got, want) {
		t.Errorf("schemas/report.estate.v1.json differs from the contract (testdata/report.shape)\n%s", lineDiff(want, got))
	}
}

// Every object is closed and requires every key; every string is capped.
func TestReportSchemaIsClosedAndCapped(t *testing.T) {
	report, _, _, _ := generated(t)
	var walk func(path string, s map[string]any)
	walk = func(path string, s map[string]any) {
		types := typeString(s["type"])
		if strings.Contains(types, "string") {
			if _, isConst := s["const"]; !isConst && s["maxLength"] != float64(512) {
				t.Errorf("%s: a string without maxLength 512", path)
			}
		}
		if props, ok := s["properties"].(map[string]any); ok {
			if s["additionalProperties"] != false {
				t.Errorf("%s: an object that is not closed", path)
			}
			var names, required []string
			for name, p := range props {
				names = append(names, name)
				walk(path+"/"+name, p.(map[string]any))
			}
			for _, r := range s["required"].([]any) {
				required = append(required, r.(string))
			}
			sort.Strings(names)
			sort.Strings(required)
			if !slices.Equal(names, required) {
				t.Errorf("%s: required %v, properties %v — every key is required", path, required, names)
			}
		} else if strings.Contains(types, "object") {
			ap, ok := s["additionalProperties"].(map[string]any)
			if !ok {
				t.Errorf("%s: an open object", path)
			} else {
				walk(path+"/{}", ap)
			}
		}
		if items, ok := s["items"].(map[string]any); ok {
			walk(path+"/[]", items)
		}
	}
	walk("", report)
}

// The Estate spec is a pure function of tequila.yaml — no revision, no timestamp — so a
// re-render is byte-identical and the applied commit is read from Flux instead.
func TestEstateSpecSourceIsRepositoryOnly(t *testing.T) {
	_, estate, reportRaw, estateRaw := generated(t)
	spec := estate["properties"].(map[string]any)["spec"].(map[string]any)
	source := spec["properties"].(map[string]any)["source"].(map[string]any)
	var keys []string
	for k := range source["properties"].(map[string]any) {
		keys = append(keys, k)
	}
	if !slices.Equal(keys, []string{"repository"}) {
		t.Errorf("spec.source has %v; it carries the repository only", keys)
	}
	crd, err := os.ReadFile(filepath.Join(moduleRoot(t), CRDPath))
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"CRD": crd, "estate schema": estateRaw, "report schema": reportRaw} {
		for _, removed := range []string{"manifestRevision", "renderedAt"} {
			if bytes.Contains(raw, []byte(removed)) {
				t.Errorf("the %s names %s — the spec carries no revision and no timestamp", name, removed)
			}
		}
	}
	if spec["additionalProperties"] != false {
		t.Error("the published Estate schema leaves spec open")
	}
}

func TestValidateCatchesWhatTheSchemaForbids(t *testing.T) {
	schema := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"a"},
		"properties": map[string]any{
			"a": map[string]any{"type": []any{"string", "null"}, "maxLength": float64(3)},
			"b": map[string]any{"type": "array", "maxItems": float64(1), "items": map[string]any{"type": "integer"}},
			"c": map[string]any{"type": "string", "enum": []any{"x", "y"}},
		},
	}
	cases := map[string]string{
		`{"a":"abc"}`:              "",
		`{"a":null}`:               "",
		`{}`:                       "lacks the required key",
		`{"a":"abcd"}`:             "characters",
		`{"a":"a","z":1}`:          "/z: is not in the schema",
		`{"a":"a","b":[1,2]}`:      "2 items",
		`{"a":"a","b":["x"]}`:      "/b/0: is string",
		`{"a":"a","c":"q"}`:        "not one of",
		`{"a":"a","b":[1.5]}`:      "/b/0: is number",
		`{"a":1}`:                  "/a: is number",
		`[1]`:                      "/: is array",
		`{"a":"a","c":"x","b":[]}`: "",
	}
	for doc, want := range cases {
		errs, err := Validate(schema, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Join(errs, "; ")
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want %q", doc, got, want)
		}
	}
}

func lineDiff(want, got []string) string {
	var b strings.Builder
	for _, w := range want {
		if !slices.Contains(got, w) {
			b.WriteString("- " + w + "\n")
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			b.WriteString("+ " + g + "\n")
		}
	}
	return b.String()
}

// The Estates this repository ships (the sample, the e2e fixture) are valid against the
// published schema — and one carrying a revision is not.
func TestShippedEstatesValidate(t *testing.T) {
	_, estate, _, _ := generated(t)
	root := moduleRoot(t)
	for _, rel := range []string{"config/samples/estate_v1alpha1_estate.yaml", "test/e2e/fixtures.yaml"} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		doc := bytes.Split(raw, []byte("\n---"))[0]
		js, err := yaml.YAMLToJSON(doc)
		if err != nil {
			t.Fatal(err)
		}
		errs, err := Validate(estate, js)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range errs {
			t.Errorf("%s: %s", rel, e)
		}
		stale := bytes.Replace(js, []byte(`"repository":"`), []byte(`"manifestRevision":"9b1c","repository":"`), 1)
		if errs, _ := Validate(estate, stale); len(errs) == 0 {
			t.Errorf("%s: spec.source.manifestRevision validates — the spec carries no revision", rel)
		}
	}
}

// The spec `tq` renders is valid against the published schema's spec — the schema an
// estate's render validates the Estate it emits with.
func TestRenderedSpecValidates(t *testing.T) {
	_, estate, _, _ := generated(t)
	spec := estate["properties"].(map[string]any)["spec"].(map[string]any)
	for _, name := range []string{"rendered-spec-dev.json", "rendered-spec-prod.json"} {
		raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "api", "v1alpha1", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		errs, err := Validate(spec, raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range errs {
			t.Errorf("%s: %s", name, e)
		}
	}
}
