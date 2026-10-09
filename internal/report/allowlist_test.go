package report_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
	"github.com/tequila/tq-operator/internal/report"
	"github.com/tequila/tq-operator/internal/schemagen"
)

func reportSchema(t *testing.T) map[string]any {
	t.Helper()
	wd, _ := os.Getwd()
	root, err := schemagen.ModuleRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, schemagen.ReportSchemaPath))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func assertValid(t *testing.T, body []byte) {
	t.Helper()
	errs, err := schemagen.Validate(reportSchema(t), body)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range errs {
		t.Errorf("the report does not validate: %s", e)
	}
}

func minimalStatus() *estatev1alpha1.EstateStatus {
	return &estatev1alpha1.EstateStatus{}
}

// fill sets every reachable field to a non-zero value: one item per list, one entry per map,
// every pointer allocated. If any type the report marshals carried a key the schema does not
// declare, the filled report would carry it.
func fill(v reflect.Value, depth int) {
	if depth > 12 {
		return
	}
	if v.Type() == reflect.TypeFor[metav1.Time]() {
		v.Set(reflect.ValueOf(metav1.NewTime(time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC))))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), depth+1)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), depth+1)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		val := reflect.New(v.Type().Elem()).Elem()
		fill(val, depth+1)
		m.SetMapIndex(reflect.ValueOf("k"), val)
		v.Set(m)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	}
}

// A report never carries a field outside the report schema: a report with every field of every
// type filled marshals to keys the schema declares — and to every key it requires.
func TestReportCarriesNothingOutsideTheSchema(t *testing.T) {
	var r report.Report
	fill(reflect.ValueOf(&r).Elem(), 0)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	errs, err := schemagen.Validate(reportSchema(t), raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range errs {
		// The filler writes "x" into enums and consts and an item into preflight; those
		// are value violations. A key the schema lacks, or a key it requires and the
		// report lacks, is the allow-list broken.
		if strings.Contains(e, "is not in the schema") || strings.Contains(e, "lacks the required key") {
			t.Errorf("allow-list: %s", e)
		}
	}
}

// The same holds through the path the operator takes: an Estate with every spec field filled
// and a status with every field filled, assembled and normalised, validates completely — no
// spec field (profile, namespaces, operator, …) leaks into the report.
func TestAssembledReportValidates(t *testing.T) {
	var est estatev1alpha1.Estate
	fill(reflect.ValueOf(&est).Elem(), 0)
	est.Spec.Product = "acme/acme"
	var st estatev1alpha1.EstateStatus
	fill(reflect.ValueOf(&st).Elem(), 0)
	st.Cluster.Platform = "k3s"
	st.Applied.Source.Kind = "GitRepository"
	st.Applied.Kustomizations[0].LastReconcile = &metav1.Time{Time: time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)}
	st.Running.Workloads[0].Kind = "Deployment"
	st.Running.Workloads[0].State = estatev1alpha1.WorkloadAttached
	st.Running.Workloads[0].Images[0].Signature = "unverified"
	st.Drift[0].Kind = estatev1alpha1.DriftRunningBehind
	st.Preflight = nil

	r := report.Assemble(&est, &st, report.Operator{Version: "0.1.0", Image: "img", Capabilities: []estatev1alpha1.Capability{"observe"}})
	r.Estate.Account, r.Estate.Environment, r.ReportedAt = "acme", "prod", "2026-10-05T14:00:00Z"
	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	assertValid(t, raw)
	for _, forbidden := range []string{"profile", "namespaces", "mongodb_in_cluster", "ownServices", "conditions", "lastReport", "observedGeneration"} {
		if strings.Contains(string(raw), `"`+forbidden+`"`) {
			t.Errorf("the report carries %q", forbidden)
		}
	}
}

// The no-Estate report (declared: null) validates too.
func TestNoEstateReportValidates(t *testing.T) {
	r := report.Assemble(nil, minimalStatus(), report.Operator{Version: "0.1.0", Image: ""})
	r.Estate.Account, r.Estate.Environment, r.ReportedAt = "acme", "prod", "2026-10-05T14:00:00Z"
	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	assertValid(t, raw)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["declared"] != nil || m["applied"] != nil {
		t.Errorf("declared %v applied %v, want null", m["declared"], m["applied"])
	}
}

// Caps and string lengths are enforced, however large the estate.
func TestNormalizeCaps(t *testing.T) {
	st := minimalStatus()
	st.Running = &estatev1alpha1.RunningStatus{}
	for range 600 {
		st.Running.Workloads = append(st.Running.Workloads, estatev1alpha1.Workload{
			Namespace: "tequila", Name: strings.Repeat("w", 900), Kind: "Deployment", Images: []estatev1alpha1.Image{},
		})
	}
	for range 250 {
		st.Drift = append(st.Drift, estatev1alpha1.Drift{Kind: estatev1alpha1.DriftNotReady, Subject: "s"})
	}
	r := report.Assemble(nil, st, report.Operator{})
	if len(r.Running.Workloads) != 500 || !r.Running.Truncated || len(r.Drift) != 200 {
		t.Errorf("workloads %d truncated %v drift %d", len(r.Running.Workloads), r.Running.Truncated, len(r.Drift))
	}
	if n := len([]rune(r.Running.Workloads[0].Name)); n != 512 {
		t.Errorf("a %d-character string survived", n)
	}
	r.Estate.Account, r.Estate.Environment, r.ReportedAt = "acme", "prod", "2026-10-05T14:00:00Z"
	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > report.MaxBodyBytes {
		t.Errorf("the body is %d bytes, over the door's %d", len(raw), report.MaxBodyBytes)
	}
	assertValid(t, raw)
}

// Who sends and when is not content: the hash ignores both.
func TestHashIgnoresIdentityAndTime(t *testing.T) {
	a := report.Assemble(nil, minimalStatus(), report.Operator{Version: "1"})
	b := a
	b.Estate.Account, b.ReportedAt = "acme", "2026-10-05T14:00:00Z"
	if a.Hash() != b.Hash() {
		t.Error("the hash depends on the identity or the time")
	}
	b.Operator.Version = "2"
	if a.Hash() == b.Hash() {
		t.Error("the hash ignores content")
	}
}
