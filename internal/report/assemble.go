package report

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"unicode/utf8"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
)

// Caps of the report schema. The status builder applies the same; Assemble re-applies them so no
// path can produce a report the schema refuses.
const (
	MaxWorkloads       = 500
	MaxDrift           = 200
	MaxKustomizations  = 200
	MaxNotReadySecrets = 100
	MaxCrashLooping    = 100
	MaxImages          = 16
	MaxArchitectures   = 8
	MaxKubeletVersions = 32
)

// Assemble builds the report from an Estate (nil when none is declared) and the observed part
// of its status. The estate identity and reportedAt are filled when the report is sent.
func Assemble(est *estatev1alpha1.Estate, st *estatev1alpha1.EstateStatus, op Operator) Report {
	r := Report{
		Schema:    SchemaID,
		Operator:  op,
		Preflight: []estatev1alpha1.PreflightResult{},
	}
	if est != nil {
		product := est.Spec.Product
		r.Estate.Product = &product
		r.Declared = declared(est)
	}
	if st.Cluster != nil {
		r.Cluster = *st.Cluster.DeepCopy()
	}
	if st.Applied != nil {
		r.Applied = st.Applied.DeepCopy()
	}
	if st.Running != nil {
		r.Running = *st.Running.DeepCopy()
	}
	if st.ExternalSecrets != nil {
		r.ExternalSecrets = *st.ExternalSecrets.DeepCopy()
	}
	for _, d := range st.Drift {
		r.Drift = append(r.Drift, *d.DeepCopy())
	}
	if st.Health != nil {
		r.Health = *st.Health.DeepCopy()
	}
	Normalize(&r)
	return r
}

func declared(est *estatev1alpha1.Estate) *Declared {
	s := est.Spec
	d := &Declared{
		Generation: est.Generation,
		Source:     DeclaredSource{Repository: s.Source.Repository},
		Renderer: DeclaredRenderer{
			Klass8sCli:    nonEmpty(s.Renderer.Klass8sCli),
			ManifestsCore: nonEmpty(s.Renderer.ManifestsCore),
			TqCli:         nonEmpty(s.Renderer.TqCli),
		},
		Platform: map[string]string{},
		Services: map[string]DeclaredService{},
	}
	for k, v := range s.Platform {
		d.Platform[k] = v
	}
	for name, svc := range s.Services {
		d.Services[name] = DeclaredService{Repo: nonEmpty(svc.Repo), Version: nonEmpty(svc.Version), Own: svc.Own}
	}
	return d
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Normalize makes a report schema-shaped: every list a list (never null), every cap applied,
// every string at most MaxStringLength characters.
func Normalize(r *Report) {
	if r.Operator.Capabilities == nil {
		r.Operator.Capabilities = []estatev1alpha1.Capability{}
	}
	r.Operator.Capabilities = capSlice(r.Operator.Capabilities, 4)
	n := &r.Cluster.Nodes
	n.Architectures = capSlice(orEmpty(n.Architectures), MaxArchitectures)
	n.KubeletVersions = capSlice(orEmpty(n.KubeletVersions), MaxKubeletVersions)
	if r.Cluster.Platform == "" {
		r.Cluster.Platform = "generic"
	}
	if r.Applied != nil {
		r.Applied.Kustomizations = capSlice(orEmpty(r.Applied.Kustomizations), MaxKustomizations)
	}
	r.Running.Workloads = orEmpty(r.Running.Workloads)
	if len(r.Running.Workloads) > MaxWorkloads {
		r.Running.Workloads = r.Running.Workloads[:MaxWorkloads]
		r.Running.Truncated = true
	}
	for i := range r.Running.Workloads {
		w := &r.Running.Workloads[i]
		w.Images = capSlice(orEmpty(w.Images), MaxImages)
		if w.Kind == "" {
			w.Kind = "Deployment"
		}
		for j := range w.Images {
			if w.Images[j].Signature == "" {
				w.Images[j].Signature = "unverified"
			}
		}
	}
	r.ExternalSecrets.NotReady = capSlice(orEmpty(r.ExternalSecrets.NotReady), MaxNotReadySecrets)
	r.Drift = capSlice(orEmpty(r.Drift), MaxDrift)
	r.Preflight = []estatev1alpha1.PreflightResult{}
	r.Health.Pods.CrashLooping = capSlice(orEmpty(r.Health.Pods.CrashLooping), MaxCrashLooping)
	if r.Declared != nil {
		if r.Declared.Platform == nil {
			r.Declared.Platform = map[string]string{}
		}
		if r.Declared.Services == nil {
			r.Declared.Services = map[string]DeclaredService{}
		}
	}
	capStrings(reflect.ValueOf(r).Elem())
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func capSlice[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// capStrings truncates every string reachable from v (struct fields, slice items, pointers,
// map values) to MaxStringLength characters.
func capStrings(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			capStrings(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				capStrings(v.Field(i))
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			capStrings(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			val := reflect.New(v.Type().Elem()).Elem()
			val.Set(v.MapIndex(k))
			capStrings(val)
			v.SetMapIndex(k, val)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(capString(v.String()))
		}
	}
}

func capString(s string) string {
	if utf8.RuneCountInString(s) <= MaxStringLength {
		return s
	}
	r := []rune(s)
	return string(r[:MaxStringLength])
}

// Hash identifies a report's content: everything but who sends it and when.
func (r Report) Hash() string {
	r.Estate.Account, r.Estate.Environment, r.ReportedAt = "", "", ""
	raw, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Marshal renders the report, halving the workload list (and saying so in running.truncated)
// until the body fits the door's limit.
func (r Report) Marshal() ([]byte, error) {
	for {
		raw, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		if len(raw) <= MaxBodyBytes || len(r.Running.Workloads) == 0 {
			return raw, nil
		}
		r.Running.Workloads = r.Running.Workloads[:len(r.Running.Workloads)/2]
		r.Running.Truncated = true
	}
}
