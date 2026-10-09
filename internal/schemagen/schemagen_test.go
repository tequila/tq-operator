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

// Every object is closed and requires every key it had at the schema's first release — a key
// added later is optional, and the contract (testdata/report.shape) names each one; every
// string is capped.
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
			for _, r := range required {
				if !slices.Contains(names, r) {
					t.Errorf("%s: requires %q, which is not a property", path, r)
				}
			}
			for _, n := range names {
				if !slices.Contains(required, n) && !slices.Contains(optionalSince, path+"/"+n) {
					t.Errorf("%s: %q is optional — every key is required unless this test names it as added after the first release", path, n)
				}
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

// optionalSince is every property a release after the first added — optional in the schema,
// always sent. Additive within v1: this list only grows.
var optionalSince = []string{
	"/applied/kustomizations/[]/suspended",
	"/applied/kustomizations/[]/attached",
	"/running/workloads/[]/state",
	"/running/workloads/[]/undeclared",
	"/running/workloads/[]/crashLooping",
	"/running/workloads/[]/restarts",
}

// A report shaped as the first release sent it — every optional property absent — validates
// against today's schema, and today's report validates against the first release's schema once
// the properties that schema lacks are removed: the schema grows additively within v1.
func TestReportSchemaIsAdditiveWithinV1(t *testing.T) {
	report, _, _, _ := generated(t)
	first := stripOptional(report)
	doc := []byte(`{"schema":"tequila.dev/report/estate/v1","estate":{"account":"acme","environment":"prod","product":null},
	 "reportedAt":"2026-10-05T14:00:00Z","operator":{"version":"0.1.0","image":"img","capabilities":["observe"]},
	 "declared":null,"cluster":{"kubernetes":"v1.35.1","platform":"generic","nodes":{"count":1,"architectures":["arm64"],"kubeletVersions":["v1.35.1"]}},
	 "applied":{"source":{"kind":"GitRepository","name":"flux-system","revision":null},
	   "kustomizations":[{"name":"services-iam","ready":true,"appliedRevision":null,"reason":"ReconciliationSucceeded","lastReconcile":null}]},
	 "running":{"estate":{"operationsK8s":null},"truncated":false,"workloads":[{"namespace":"tequila","name":"iam-api","kind":"Deployment","service":"iam",
	   "replicas":{"desired":1,"ready":1},"images":[{"ref":"registry.example.com/platform/iam-api:v1","digest":null,"signature":"unverified"}]}]},
	 "externalSecrets":{"total":0,"ready":0,"notReady":[]},"drift":[],"preflight":[],"health":{"pods":{"running":1,"pending":0,"failed":0,"crashLooping":[]}}}`)
	for name, schema := range map[string]map[string]any{"today's schema": report, "the first release's schema": first} {
		errs, err := Validate(schema, doc)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range errs {
			t.Errorf("a first-release report against %s: %s", name, e)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	k := m["applied"].(map[string]any)["kustomizations"].([]any)[0].(map[string]any)
	k["suspended"], k["attached"] = true, true
	w := m["running"].(map[string]any)["workloads"].([]any)[0].(map[string]any)
	w["state"], w["undeclared"], w["crashLooping"], w["restarts"] = "attached", false, true, float64(3)
	today, _ := json.Marshal(m)
	if errs, _ := Validate(report, today); len(errs) > 0 {
		t.Errorf("today's report against today's schema: %v", errs)
	}
	if errs, _ := Validate(first, today); len(errs) == 0 {
		t.Error("today's report validates against the first release's schema with the new properties present — the schema is closed, so this must fail")
	}
	for _, key := range []string{"suspended", "attached"} {
		delete(k, key)
	}
	for _, key := range []string{"state", "undeclared", "crashLooping", "restarts"} {
		delete(w, key)
	}
	stripped, _ := json.Marshal(m)
	if errs, _ := Validate(first, stripped); len(errs) > 0 {
		t.Errorf("today's report minus the new properties against the first release's schema: %v", errs)
	}
}

// stripOptional returns the schema without any property its object does not require — the
// schema as the first release published it.
func stripOptional(s map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range s {
		out[k] = v
	}
	if props, ok := s["properties"].(map[string]any); ok {
		required := map[string]bool{}
		for _, r := range s["required"].([]any) {
			required[r.(string)] = true
		}
		kept := map[string]any{}
		for name, p := range props {
			if required[name] {
				kept[name] = stripOptional(p.(map[string]any))
			}
		}
		out["properties"] = kept
	}
	if items, ok := s["items"].(map[string]any); ok {
		out["items"] = stripOptional(items)
	}
	if ap, ok := s["additionalProperties"].(map[string]any); ok {
		out["additionalProperties"] = stripOptional(ap)
	}
	return out
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
