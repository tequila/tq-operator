package controller

import (
	"strings"
	"testing"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
	"github.com/tequila/tq-operator/internal/observe"
)

func spec() *estatev1alpha1.EstateSpec {
	return &estatev1alpha1.EstateSpec{
		Namespaces: estatev1alpha1.EstateNamespaces{Platform: "operations", Services: "tequila"},
		Services: map[string]estatev1alpha1.EstateService{
			"iam":              {Repo: "tequila/iam", Version: "v0.45.4"},
			"media":            {Repo: "tequila/media", Version: "v1.0.0"},
			"media-conversion": {Repo: "tequila/media-conversion", Version: "v0.3.0"},
			"backend":          {Own: true},
		},
	}
}

var scope = observe.Scope{Platform: "operations", Services: "tequila"}

func w(ns, name, label string, desired, ready int32, images ...string) observe.Workload {
	out := observe.Workload{Namespace: ns, Name: name, NameLabel: label, Desired: desired, Ready: ready}
	for _, img := range images {
		out.Images = append(out.Images, observe.Image{Container: "app", Ref: img})
	}
	return out
}

func TestImageServiceAndTag(t *testing.T) {
	services := declaredVersions(spec())
	cases := []struct{ ref, service, tag string }{
		{"registry.tequila.dev/platform/iam-api:v0.45.4", "iam", "v0.45.4"},
		{"428477779568.dkr.ecr.eu-central-1.amazonaws.com/platform/iam-worker:v0.45.3", "iam", "v0.45.3"},
		{"registry.tequila.dev/platform/media-conversion-worker:v0.3.0", "media-conversion", "v0.3.0"},
		{"registry.tequila.dev/platform/media-api:v1.0.0@sha256:abc", "media", "v1.0.0"},
		{"registry.tequila.dev/acme/iam-api:v1", "", "v1"},
		{"registry.tequila.dev/platform/backend-api:v1", "", "v1"},
		{"ghcr.io/example-org/platform/iam-api:v2", "iam", "v2"},
		{"localhost:5000/platform/iam-api", "iam", ""},
		{"platform/iam", "iam", ""},
	}
	for _, c := range cases {
		if got := imageService(c.ref, services); got != c.service {
			t.Errorf("imageService(%s) = %q, want %q", c.ref, got, c.service)
		}
		if got := imageTag(c.ref); got != c.tag {
			t.Errorf("imageTag(%s) = %q, want %q", c.ref, got, c.tag)
		}
	}
	// The path decides what a platform image is, never the host.
	for ref, want := range map[string]bool{
		"registry.tequila.dev/platform/ghost-api:v1":              true,
		"123456789012.dkr.ecr.eu-west-1.amazonaws.com/platform/x": true,
		"localhost:5000/platform/x@sha256:abc":                    true,
		"platform/x":                                              true,
		"registry.tequila.dev/acme/platform-api:v1":               false,
		"registry.tequila.dev/platform:v1":                        false,
		"registry.example.com/example-org/x:v1":                   false,
	} {
		if got := isPlatformImage(ref); got != want {
			t.Errorf("isPlatformImage(%s) = %v, want %v", ref, got, want)
		}
	}
}

// A workload in the services namespace running a platform image no declared service accounts
// for is reported as undeclared — whatever ownServices says — and is an Undeclared drift; one
// a declared service's name accounts for by its image is not, nor is anything in the platform
// namespace, nor a tenant's own image.
func TestUndeclaredPlatformWorkloads(t *testing.T) {
	obs := &observe.Observation{Workloads: []observe.Workload{
		w("operations", "tq-operator", "tq-operator", 1, 1, "registry.tequila.dev/platform/tq-operator:v0.2.0"),
		w("tequila", "ghost-api", "ghost", 1, 1, "registry.example.com/platform/ghost-api:v1.0.0"),
		w("tequila", "iam-mailer", "", 1, 1, "registry.tequila.dev/platform/iam-mailer:v0.45.4"),
		w("tequila", "backend-api", "backend", 1, 1, "registry.example.com/acme/backend-api:v1"),
		w("tequila", "sidecar", "", 1, 1, "registry.example.com/acme/sidecar:v1"),
	}}
	for _, ownServices := range []bool{true, false} {
		st := Observed(spec(), obs, scope, ownServices)
		undeclared := map[string]bool{}
		for _, wl := range st.Running.Workloads {
			undeclared[wl.Name] = wl.Undeclared
		}
		if !undeclared["ghost-api"] {
			t.Errorf("ownServices %v: ghost-api is not undeclared", ownServices)
		}
		if undeclared["tq-operator"] || undeclared["iam-mailer"] || undeclared["backend-api"] || undeclared["sidecar"] {
			t.Errorf("ownServices %v: undeclared = %v", ownServices, undeclared)
		}
		var drift []string
		for _, d := range st.Drift {
			drift = append(drift, string(d.Kind)+" "+d.Subject+" "+deref(d.Declared)+"→"+deref(d.Observed))
		}
		if want := "Undeclared tequila/ghost-api —→registry.example.com/platform/ghost-api:v1.0.0"; strings.Join(drift, "|") != want {
			t.Errorf("ownServices %v: drift %q, want %q", ownServices, strings.Join(drift, "|"), want)
		}
		if _, present := undeclared["sidecar"]; present != ownServices {
			t.Errorf("ownServices %v: a tenant's own unaccounted-for workload reported: %v", ownServices, present)
		}
	}
	// No Estate: nothing is undeclared, there is nothing to be declared in.
	for _, wl := range Observed(nil, obs, scope, true).Running.Workloads {
		if wl.Undeclared {
			t.Errorf("no Estate, yet %s is undeclared", wl.Name)
		}
	}
}

// A developer's checkout running over a workload — its Kustomization suspended and annotated —
// reports state attached and is never drift: not RunningBehind, not NotReady, not Undeclared;
// nor is the Kustomization AppliedBehind or NotReady. A suspended Kustomization without the
// annotation changes nothing.
func TestAttachedWorkloadsAreNeverDrift(t *testing.T) {
	a, b := "main@sha1:aaa", "main@sha1:bbb"
	attachedWorkload := w("tequila", "iam-worker", "iam", 1, 0, "registry.tequila.dev/platform/iam-worker:dev-abc123")
	attachedWorkload.Kustomization = "services-iam-worker"
	attachedGhost := w("tequila", "ghost-api", "ghost", 1, 0, "registry.example.com/platform/ghost-api:v1.0.0")
	attachedGhost.Kustomization = "services-iam-worker"
	suspendedOnly := w("tequila", "iam-api", "iam", 1, 0, "registry.tequila.dev/platform/iam-api:v0.45.3")
	suspendedOnly.Kustomization = "services-iam-api"
	obs := &observe.Observation{
		Workloads: []observe.Workload{attachedWorkload, attachedGhost, suspendedOnly},
		Flux: &observe.Flux{
			Sources: map[string]*string{"GitRepository/flux-system": &b},
			Kustomizations: []observe.Kustomization{
				{Name: "services-iam-api", SourceKey: "GitRepository/flux-system", Suspended: true, AppliedRevision: &a, ReadyStatus: "False", Reason: "HealthCheckFailed"},
				{Name: "services-iam-worker", SourceKey: "GitRepository/flux-system", Suspended: true, AttachAnnotated: true, AppliedRevision: &a, ReadyStatus: "False", Reason: "HealthCheckFailed"},
				{Name: "services-stale", SourceKey: "GitRepository/flux-system", AttachAnnotated: true, AppliedRevision: &a, ReadyStatus: "True"},
			},
			Primary: "GitRepository/flux-system",
		},
	}
	st := Observed(spec(), obs, scope, true)
	states := map[string]estatev1alpha1.WorkloadState{}
	for _, wl := range st.Running.Workloads {
		states[wl.Name] = wl.State
	}
	if states["iam-worker"] != estatev1alpha1.WorkloadAttached || states["ghost-api"] != estatev1alpha1.WorkloadAttached || states["iam-api"] != estatev1alpha1.WorkloadManaged {
		t.Errorf("states %v", states)
	}
	var got []string
	for _, d := range st.Drift {
		got = append(got, string(d.Kind)+" "+d.Subject)
	}
	// Only the suspended-without-annotation unit and its workload drift, as before.
	want := "RunningBehind tequila/iam-api|AppliedBehind services-iam-api|AppliedBehind services-stale|NotReady services-iam-api|NotReady tequila/iam-api"
	if strings.Join(got, "|") != want {
		t.Errorf("drift\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}
	byName := map[string]estatev1alpha1.KustomizationStatus{}
	for _, k := range st.Applied.Kustomizations {
		byName[k.Name] = k
	}
	if k := byName["services-iam-worker"]; !k.Suspended || !k.Attached {
		t.Errorf("services-iam-worker %+v", k)
	}
	if k := byName["services-iam-api"]; !k.Suspended || k.Attached {
		t.Errorf("services-iam-api %+v", k)
	}
	if k := byName["services-stale"]; k.Suspended || k.Attached {
		t.Errorf("a stale annotation on a resumed Kustomization attaches nothing: %+v", k)
	}
}

// A crash loop is on the workload (crashLooping, the restart sum) and in health — not drift.
func TestCrashLoopsAreOnTheWorkload(t *testing.T) {
	crasher := w("tequila", "iam-worker", "iam", 1, 1, "registry.tequila.dev/platform/iam-worker:v0.45.4")
	crasher.CrashLooping, crasher.Restarts = true, 7
	obs := &observe.Observation{
		Workloads: []observe.Workload{crasher, w("tequila", "iam-api", "iam", 1, 1, "registry.tequila.dev/platform/iam-api:v0.45.4")},
		Pods:      observe.PodSummary{Running: 2, CrashLooping: []string{"tequila/iam-worker"}},
	}
	st := Observed(spec(), obs, scope, true)
	for _, wl := range st.Running.Workloads {
		switch wl.Name {
		case "iam-worker":
			if !wl.CrashLooping || wl.Restarts != 7 {
				t.Errorf("iam-worker %+v", wl)
			}
		case "iam-api":
			if wl.CrashLooping || wl.Restarts != 0 {
				t.Errorf("iam-api %+v", wl)
			}
		}
	}
	if len(st.Drift) != 0 {
		t.Errorf("a crash loop is not drift: %+v", st.Drift)
	}
	if got := strings.Join(st.Health.Pods.CrashLooping, ","); got != "tequila/iam-worker" {
		t.Errorf("health.crashLooping %s", got)
	}
}

func TestObservedAttributesFiltersAndDrifts(t *testing.T) {
	obs := &observe.Observation{
		Workloads: []observe.Workload{
			w("operations", "kgateway", "kgateway", 1, 1, "cr.kgateway.dev/kgateway:v2.1.0"),
			w("tequila", "backend-api", "backend", 1, 0, "registry.tequila.dev/acme/backend-api:v1"),
			w("tequila", "iam-api", "iam", 2, 2, "registry.tequila.dev/platform/iam-api:v0.45.4"),
			w("tequila", "iam-worker", "iam", 1, 1, "registry.tequila.dev/platform/iam-worker:v0.45.3"),
			w("tequila", "mystery", "", 1, 1, "registry.tequila.dev/acme/mystery:v9"),
		},
		Pods: observe.PodSummary{Running: 5, CrashLooping: []string{"tequila/backend-api", "tequila/iam-worker", "tequila/mystery-job"}},
		ExternalSecrets: []observe.ExternalSecret{
			{Namespace: "tequila", Name: "a", Ready: true},
			{Namespace: "tequila", Name: "b", Ready: false, Reason: ""},
		},
	}

	st := Observed(spec(), obs, scope, true)
	names := func(st estatev1alpha1.EstateStatus) string {
		var out []string
		for _, w := range st.Running.Workloads {
			s := "-"
			if w.Service != nil {
				s = *w.Service
			}
			out = append(out, w.Name+"="+s)
		}
		return strings.Join(out, " ")
	}
	if got := names(st); got != "kgateway=- backend-api=- iam-api=iam iam-worker=iam mystery=-" {
		t.Errorf("ownServices true: %s", got)
	}
	var drift []string
	for _, d := range st.Drift {
		drift = append(drift, string(d.Kind)+" "+d.Subject+" "+deref(d.Declared)+"→"+deref(d.Observed))
	}
	want := "RunningBehind tequila/iam-worker v0.45.4→v0.45.3|NotReady tequila/backend-api 1→0|SecretNotResolvable tequila/b —→Unknown"
	if strings.Join(drift, "|") != want {
		t.Errorf("drift:\n%s\nwant\n%s", strings.Join(drift, "|"), want)
	}
	if st.ExternalSecrets.Total != 2 || st.ExternalSecrets.Ready != 1 || st.ExternalSecrets.NotReady[0].Reason != "Unknown" {
		t.Errorf("externalSecrets %+v", st.ExternalSecrets)
	}

	// ownServices false: the tenant's own and the unaccounted-for go — from running, drift and
	// the crash-loop names alike.
	st = Observed(spec(), obs, scope, false)
	if got := names(st); got != "kgateway=- iam-api=iam iam-worker=iam" {
		t.Errorf("ownServices false: %s", got)
	}
	for _, d := range st.Drift {
		if strings.Contains(d.Subject, "backend") || strings.Contains(d.Subject, "mystery") {
			t.Errorf("drift names a workload the report leaves out: %+v", d)
		}
	}
	if got := strings.Join(st.Health.Pods.CrashLooping, ","); got != "tequila/iam-worker" {
		t.Errorf("crashLooping %s", got)
	}

	// No Estate: one NoEstate drift, nothing attributed.
	st = Observed(nil, obs, scope, true)
	if st.Drift[0].Kind != estatev1alpha1.DriftNoEstate {
		t.Errorf("no Estate: %+v", st.Drift)
	}
	for _, w := range st.Running.Workloads {
		if w.Service != nil {
			t.Errorf("no Estate, yet %s is attributed", w.Name)
		}
	}
}

func TestFluxDrift(t *testing.T) {
	a, b := "main@sha1:aaa", "main@sha1:bbb"
	obs := &observe.Observation{Flux: &observe.Flux{
		Sources: map[string]*string{"GitRepository/flux-system": &b},
		Kustomizations: []observe.Kustomization{
			{Name: "services-iam", SourceKey: "GitRepository/flux-system", AppliedRevision: &a, ReadyStatus: "True"},
			{Name: "platform-gateway", SourceKey: "GitRepository/flux-system", AppliedRevision: &b, ReadyStatus: "False", Reason: "HealthCheckFailed"},
			{Name: "platform-nats", SourceKey: "GitRepository/flux-system", AppliedRevision: &b, ReadyStatus: "Unknown", Reason: "Progressing"},
			{Name: "services-new", SourceKey: "GitRepository/flux-system", ReadyStatus: ""},
		},
		InstancesNotReady: map[string]string{"flux-system/flux": "ReconciliationFailed"},
		Primary:           "GitRepository/flux-system",
	}}
	st := Observed(spec(), obs, scope, true)
	var got []string
	for _, d := range st.Drift {
		got = append(got, string(d.Kind)+" "+d.Subject)
	}
	want := "AppliedBehind services-iam|NotReady flux-system/flux|NotReady platform-gateway"
	if strings.Join(got, "|") != want {
		t.Errorf("drift %s, want %s", strings.Join(got, "|"), want)
	}
	if st.Applied == nil || st.Applied.Source.Name != "flux-system" || len(st.Applied.Kustomizations) != 4 {
		t.Fatalf("applied %+v", st.Applied)
	}
	if st.Applied.Kustomizations[2].Ready || st.Applied.Kustomizations[2].Name != "platform-nats" {
		t.Errorf("Unknown is not ready: %+v", st.Applied.Kustomizations[2])
	}
	reason, msg := summarize(st.Drift)
	if reason != "AppliedBehind" || !strings.Contains(msg, "1 AppliedBehind, 2 NotReady") {
		t.Errorf("summary %s: %s", reason, msg)
	}
	if reason, _ := summarize(nil); reason != "InSync" {
		t.Errorf("no drift: %s", reason)
	}
}
