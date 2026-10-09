package controller_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
	"github.com/tequila/tq-operator/internal/controller"
	"github.com/tequila/tq-operator/internal/observe"
	"github.com/tequila/tq-operator/internal/report"
	"github.com/tequila/tq-operator/internal/schemagen"
)

// The envtest suite: a real API server and etcd (no kubelet, no controllers), the Estate CRD
// and stand-ins for Flux's and ESO's, fake Flux objects and Deployments — and the operator's
// manager, built the way cmd/main.go builds it, reporting to an httptest IAM and console.

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var namespaces = observe.Namespaces{Own: "tq-operator", Platform: "operations", Services: "tequila", Flux: "flux-system"}

func startEnv(t *testing.T) (*envtest.Environment, client.Client, *runtime.Scheme) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set — run `make test` (setup-envtest provides the API server and etcd)")
	}
	root, err := schemagen.ModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases"), filepath.Join(root, "test", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, estatev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return env, c, scheme
}

// fakeTequila answers the exchange and the report door, recording what reached it.
type fakeTequila struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  [][]byte
	paths   []string
	unavail bool
}

func newFakeTequila(t *testing.T) *fakeTequila {
	f := &fakeTequila{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/oauth/token":
			enc := base64.RawURLEncoding.EncodeToString
			claims, _ := json.Marshal(map[string]any{"sub": "estate:acme/prod", "exp": time.Now().Add(10 * time.Minute).Unix()})
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": enc([]byte(`{}`)) + "." + enc(claims) + "." + enc([]byte("s")), "expires_in": 600})
		case strings.HasPrefix(r.URL.Path, "/api/v1/estates/"):
			if f.unavail {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.bodies, f.paths = append(f.bodies, body), append(f.paths, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTequila) last() ([]byte, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil, "", 0
	}
	return f.bodies[len(f.bodies)-1], f.paths[len(f.paths)-1], len(f.bodies)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func deployment(ns, name, service string, replicas, ready int32, image string, labels map[string]string) *appsv1.Deployment {
	sel := map[string]string{"app.kubernetes.io/name": service, "app.kubernetes.io/component": name}
	meta := map[string]string{"app.kubernetes.io/name": service}
	for k, v := range labels {
		meta[k] = v
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: meta},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "app", Image: image,
					Env: []corev1.EnvVar{{Name: "DATABASE_PASSWORD", Value: "never-reported"}},
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{Replicas: replicas, ReadyReplicas: ready},
	}
}

func createWithStatus(t *testing.T, ctx context.Context, c client.Client, obj client.Object, status func()) {
	t.Helper()
	must(t, c.Create(ctx, obj))
	if status != nil {
		status()
		must(t, c.Status().Update(ctx, obj))
	}
}

func unstructuredObj(gvk schema.GroupVersionKind, ns, name string, spec, status map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	if status != nil {
		u.Object["status"] = status
	}
	return u
}

func seed(t *testing.T, ctx context.Context, c client.Client) {
	for _, ns := range []string{"tq-operator", "operations", "tequila", "flux-system"} {
		must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
		must(t, c.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "default"}}))
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a-secret-name"}}
	createWithStatus(t, ctx, c, node, func() {
		node.Status.NodeInfo = corev1.NodeSystemInfo{Architecture: "arm64", KubeletVersion: "v1.35.1+k3s1", OperatingSystem: "linux"}
	})
	for _, d := range []*appsv1.Deployment{
		deployment("tequila", "iam-api", "iam", 2, 2, "registry.tequila.dev/platform/iam-api:v0.45.4", nil),
		deployment("tequila", "iam-worker", "iam", 1, 1, "registry.tequila.dev/platform/iam-worker:v0.45.3", nil),
		deployment("tequila", "backend-api", "backend", 1, 1, "registry.tequila.dev/acme/backend-api:v1.2.3", nil),
		// A developer's checkout runs over iam-mailer: its Kustomization (below) is suspended and
		// annotated. Behind on its version and not ready — and never drift.
		deployment("tequila", "iam-mailer", "iam", 1, 0, "registry.tequila.dev/platform/iam-mailer:dev-local",
			map[string]string{"kustomize.toolkit.fluxcd.io/name": "services-iam-mailer", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}),
		// A platform image no declared service accounts for.
		deployment("tequila", "ghost-api", "ghost", 1, 1, "registry.tequila.dev/platform/ghost-api:v1.0.0", nil),
		deployment("operations", "kgateway", "kgateway", 1, 1, "cr.kgateway.dev/kgateway:v2.1.0", map[string]string{"estate.tequila.dev/operations-k8s": "v0.50.0"}),
	} {
		status := d.Status
		createWithStatus(t, ctx, c, d, func() { d.Status = status })
	}
	running := func(name, component, service, image, imageID string, waiting string) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tequila", Name: name,
				Labels: map[string]string{"app.kubernetes.io/name": service, "app.kubernetes.io/component": component}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}},
		}
		createWithStatus(t, ctx, c, p, func() {
			cs := corev1.ContainerStatus{Name: "app", Image: image, ImageID: imageID, Ready: waiting == ""}
			if waiting != "" {
				cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: waiting, Message: "back-off restarting"}
				cs.RestartCount = 7
			}
			p.Status.Phase = corev1.PodRunning
			p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
		})
		return p
	}
	running("iam-api-7d9f-abcde", "iam-api", "iam", "registry.tequila.dev/platform/iam-api:v0.45.4", "registry.tequila.dev/platform/iam-api@"+digest, "")
	running("iam-worker-5c4b-fghij", "iam-worker", "iam", "registry.tequila.dev/platform/iam-worker:v0.45.3", "", "CrashLoopBackOff")

	flux := []*unstructured.Unstructured{
		unstructuredObj(observe.GitRepositoryGVK, "flux-system", "flux-system", map[string]any{"url": "https://github.com/acme/gitops.git"},
			map[string]any{"artifact": map[string]any{"revision": "main@sha1:bbb"}}),
		unstructuredObj(observe.KustomizationGVK, "flux-system", "services-iam",
			map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}, "path": "./services/iam/prod/iam"},
			map[string]any{"lastAppliedRevision": "main@sha1:aaa", "conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded", "message": "Applied revision: main@sha1:aaa",
				"lastTransitionTime": "2026-10-05T13:00:00Z"}}}),
		unstructuredObj(observe.KustomizationGVK, "flux-system", "platform-gateway",
			map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}},
			map[string]any{"lastAppliedRevision": "main@sha1:bbb", "conditions": []any{map[string]any{
				"type": "Ready", "status": "False", "reason": "HealthCheckFailed", "message": "timeout waiting for kgateway",
				"lastTransitionTime": "2026-10-05T13:30:00Z"}}}),
		attached(unstructuredObj(observe.KustomizationGVK, "flux-system", "services-iam-mailer",
			map[string]any{"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}, "suspend": true},
			map[string]any{"lastAppliedRevision": "main@sha1:aaa", "conditions": []any{map[string]any{
				"type": "Ready", "status": "False", "reason": "HealthCheckFailed", "message": "timeout waiting for iam-mailer",
				"lastTransitionTime": "2026-10-05T13:30:00Z"}}})),
		unstructuredObj(observe.ExternalSecretGVK, "tequila", "iam-app-secrets",
			map[string]any{"data": []any{map[string]any{"secretKey": "K", "remoteRef": map[string]any{"key": "remote-key-name"}}}},
			map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "SecretSyncedError", "message": "could not get secret"}}}),
		unstructuredObj(observe.ExternalSecretGVK, "tequila", "iam-jwt",
			map[string]any{}, map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "SecretSynced"}}}),
	}
	for _, u := range flux {
		status := u.Object["status"]
		must(t, c.Create(ctx, u))
		u.Object["status"] = status
		must(t, c.Status().Update(ctx, u))
	}
}

// attached marks a Kustomization the way a developer's inner loop does when it suspends the
// unit to run a local checkout over its workloads.
func attached(u *unstructured.Unstructured) *unstructured.Unstructured {
	u.SetAnnotations(map[string]string{
		"tequila.dev/attached-by":               "developer@example-org",
		"tequila.dev/attached-from":             "/home/developer/src/iam",
		"tequila.dev/attached-since":            "2026-10-05T12:00:00Z",
		"kustomize.toolkit.fluxcd.io/reconcile": "disabled",
	})
	return u
}

func estate() *estatev1alpha1.Estate {
	return &estatev1alpha1.Estate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tq-operator", Name: "prod"},
		Spec: estatev1alpha1.EstateSpec{
			Product:     "acme/acme",
			Environment: "prod",
			Source:      estatev1alpha1.EstateSource{Repository: "acme/gitops"},
			Renderer:    estatev1alpha1.EstateRenderer{Klass8sCli: "v0.1.1", ManifestsCore: "v0.23.0", TqCli: "v0.72.0"},
			Platform:    map[string]string{"operations-k8s": "v0.50.0"},
			Profile:     estatev1alpha1.EstateProfile{Cloud: "generic", NatsInCluster: true},
			Namespaces:  estatev1alpha1.EstateNamespaces{Platform: "operations", Services: "tequila"},
			Services: map[string]estatev1alpha1.EstateService{
				"iam":     {Repo: "tequila/iam", Version: "v0.45.4"},
				"backend": {Own: true},
			},
			Operator: estatev1alpha1.OperatorSpec{Capabilities: []estatev1alpha1.Capability{"observe"}},
		},
	}
}

func startManager(t *testing.T, env *envtest.Environment, scheme *runtime.Scheme, fake *fakeTequila) {
	t.Helper()
	direct, err := client.New(env.Config, client.Options{Scheme: scheme})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	kinds := observe.Probe(ctx, direct, direct.RESTMapper(), namespaces)
	for _, gvk := range observe.AllKinds() {
		if !kinds.Observed(gvk) {
			t.Fatalf("probe: %s is %s (%s)", gvk.Kind, kinds[gvk].Access, kinds[gvk].Detail)
		}
	}
	sync := 15 * time.Minute
	mgr, err := ctrl.NewManager(env.Config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  controller.CacheOptions(namespaces, kinds, &sync),
		Client:                 client.Options{Cache: &client.CacheOptions{Unstructured: true}},
	})
	must(t, err)
	disc, err := discovery.NewDiscoveryClientForConfig(env.Config)
	must(t, err)
	u, _ := url.Parse(fake.URL)
	tokenFile := filepath.Join(t.TempDir(), "token")
	must(t, os.WriteFile(tokenFile, []byte("a.b.c"), 0o600))
	r := &controller.EstateReconciler{
		Client: mgr.GetClient(),
		Observer: &observe.Observer{Reader: mgr.GetClient(), Kinds: kinds, FluxNamespace: "flux-system",
			ServerVersion: func(context.Context) (string, error) {
				v, err := disc.ServerVersion()
				if err != nil {
					return "", err
				}
				return v.GitVersion, nil
			}},
		Reporter: &report.Reporter{Client: &report.Client{IAM: u, Console: u, TokenFile: tokenFile, HTTP: fake.Client()}, Started: time.Now()},
		Recorder: mgr.GetEventRecorderFor("tq-operator"), //nolint:staticcheck // core/v1 Events, as in cmd/main.go
		Config: controller.Config{
			Namespaces: namespaces, Interval: 15 * time.Minute, ReportOwnServices: true,
			Operator: report.Operator{Version: "0.1.0", Image: "registry.tequila.dev/platform/tq-operator:v0.1.0", Capabilities: []estatev1alpha1.Capability{"observe"}},
		},
	}
	must(t, r.SetupWithManager(mgr, kinds))
	go func() { _ = mgr.Start(ctx) }()
}

func waitFor(t *testing.T, c client.Client, what string, cond func(*estatev1alpha1.Estate) bool) *estatev1alpha1.Estate {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var est estatev1alpha1.Estate
	for time.Now().Before(deadline) {
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tq-operator", Name: "prod"}, &est); err == nil && cond(&est) {
			return &est
		}
		time.Sleep(250 * time.Millisecond)
	}
	raw, _ := json.MarshalIndent(est.Status, "", "  ")
	t.Fatalf("timed out waiting for %s; status:\n%s", what, raw)
	return nil
}

func condition(est *estatev1alpha1.Estate, typ string) *metav1.Condition {
	return meta.FindStatusCondition(est.Status.Conditions, typ)
}

// neverGatesFlux holds the Estate's status as the API server stores it to what Flux's health
// check must never wait on: no top-level observedGeneration and no condition of type Ready.
func neverGatesFlux(t *testing.T, c client.Client) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(estatev1alpha1.GroupVersion.WithKind("Estate"))
	must(t, c.Get(context.Background(), types.NamespacedName{Namespace: "tq-operator", Name: "prod"}, u))
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "observedGeneration"); found {
		t.Error("status.observedGeneration is stored: Flux would wait for the operator")
	}
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, cond := range conditions {
		if m, ok := cond.(map[string]any); ok && m["type"] == "Ready" {
			t.Errorf("a condition of type Ready is stored: %v", m)
		}
	}
}

func TestEstateReconcilerObservesComparesAndReports(t *testing.T) {
	env, c, scheme := startEnv(t)
	ctx := context.Background()
	seed(t, ctx, c)
	first := estate()
	must(t, c.Create(ctx, first))
	// A Ready: False an earlier build left: the reconciler's first status write drops it.
	first.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "EarlierBuild",
		Message: "left by an earlier build", LastTransitionTime: metav1.Now()}}
	must(t, c.Status().Update(ctx, first))
	fake := newFakeTequila(t)
	startManager(t, env, scheme, fake)

	est := waitFor(t, c, "Reported: True", func(e *estatev1alpha1.Estate) bool {
		r := condition(e, controller.ConditionReported)
		return r != nil && r.Status == metav1.ConditionTrue
	})

	// Conditions: Observed (never Ready), each recording the generation it was set for.
	neverGatesFlux(t, c)
	if r := condition(est, controller.ConditionObserved); r == nil || r.Status != metav1.ConditionTrue || r.Reason != controller.ReasonReconciled || r.ObservedGeneration != est.Generation {
		t.Errorf("Observed = %+v (generation %d)", r, est.Generation)
	}
	if r := condition(est, controller.ConditionReported); r.Reason != "Accepted" {
		t.Errorf("Reported = %+v", r)
	}
	if d := condition(est, controller.ConditionDrifted); d == nil || d.Status != metav1.ConditionTrue || d.Reason != "RunningBehind" {
		t.Errorf("Drifted = %+v", d)
	}
	if est.Status.LastReport == nil || est.Status.LastReport.Outcome != "accepted" || est.Status.LastReport.Status != 202 {
		t.Errorf("lastReport %+v", est.Status.LastReport)
	}

	// Cluster: versions and architectures, never a node's name.
	cl := est.Status.Cluster
	if cl.Platform != "k3s" || cl.Nodes.Count != 1 || strings.Join(cl.Nodes.Architectures, ",") != "arm64" || cl.Kubernetes == "" {
		t.Errorf("cluster = %+v", cl)
	}

	// Applied: the flux-system source and the three Kustomizations — the attached one says so.
	ap := est.Status.Applied
	if ap == nil || ap.Source.Kind != "GitRepository" || ap.Source.Name != "flux-system" || *ap.Source.Revision != "main@sha1:bbb" || len(ap.Kustomizations) != 3 {
		t.Fatalf("applied = %+v", ap)
	}
	for _, k := range ap.Kustomizations {
		if want := k.Name == "services-iam-mailer"; k.Suspended != want || k.Attached != want {
			t.Errorf("kustomization %+v", k)
		}
	}

	// Running: six workloads; attribution; the digest the Pod pulled; the estate's version;
	// the attached, the undeclared and the crash-looping one.
	byName := map[string]estatev1alpha1.Workload{}
	for _, w := range est.Status.Running.Workloads {
		byName[w.Namespace+"/"+w.Name] = w
	}
	if len(byName) != 6 {
		t.Errorf("workloads = %v", byName)
	}
	if w := byName["tequila/iam-api"]; w.Service == nil || *w.Service != "iam" || w.Replicas.Desired != 2 || w.Images[0].Digest == nil || *w.Images[0].Digest != digest || w.Images[0].Signature != "unverified" || w.State != "managed" || w.Undeclared || w.CrashLooping || w.Restarts != 0 {
		t.Errorf("iam-api = %+v", w)
	}
	if w := byName["tequila/backend-api"]; w.Service != nil {
		t.Errorf("a tenant's own service carries no service name: %+v", w)
	}
	if w := byName["tequila/iam-mailer"]; w.State != "attached" || w.Service == nil || *w.Service != "iam" {
		t.Errorf("iam-mailer = %+v", w)
	}
	if w := byName["tequila/ghost-api"]; !w.Undeclared || w.Service != nil || w.State != "managed" {
		t.Errorf("ghost-api = %+v", w)
	}
	if w := byName["tequila/iam-worker"]; !w.CrashLooping || w.Restarts != 7 {
		t.Errorf("iam-worker = %+v", w)
	}
	if v := est.Status.Running.Estate.OperationsK8s; v == nil || *v != "v0.50.0" {
		t.Errorf("running.estate = %v", v)
	}

	// External secrets and health.
	if es := est.Status.ExternalSecrets; es.Total != 2 || es.Ready != 1 || len(es.NotReady) != 1 || es.NotReady[0].Reason != "SecretSyncedError" {
		t.Errorf("externalSecrets = %+v", es)
	}
	if h := est.Status.Health.Pods; h.Running != 2 || strings.Join(h.CrashLooping, ",") != "tequila/iam-worker" {
		t.Errorf("health = %+v", h)
	}

	// Drift: every kind rung observe produces, in order — and nothing about the attached
	// iam-mailer or its Kustomization, though both are behind and not ready.
	var got []string
	for _, d := range est.Status.Drift {
		got = append(got, string(d.Kind)+" "+d.Subject)
	}
	want := []string{
		"RunningBehind tequila/iam-worker",
		"AppliedBehind services-iam",
		"NotReady platform-gateway",
		"SecretNotResolvable tequila/iam-app-secrets",
		"Undeclared tequila/ghost-api",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("drift =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The report: addressed by the identity, valid against the schema, nothing beyond it.
	body, path, _ := fake.last()
	if path != "/api/v1/estates/acme/prod/report" {
		t.Errorf("path %q", path)
	}
	schemaRaw, err := os.ReadFile(filepath.Join("..", "..", schemagen.ReportSchemaPath))
	must(t, err)
	var reportSchema map[string]any
	must(t, json.Unmarshal(schemaRaw, &reportSchema))
	errs, err := schemagen.Validate(reportSchema, body)
	must(t, err)
	for _, e := range errs {
		t.Errorf("report: %s", e)
	}
	for _, never := range []string{"never-reported", "DATABASE_PASSWORD", "node-a-secret-name", "iam-api-7d9f-abcde", "remote-key-name", "timeout waiting", "could not get secret",
		"developer@example-org", "/home/developer", "attached-by"} {
		if strings.Contains(string(body), never) {
			t.Errorf("the report carries %q", never)
		}
	}
	// The report's blocks are the status, byte for byte: what the cluster shows is what was sent.
	var sent map[string]json.RawMessage
	must(t, json.Unmarshal(body, &sent))
	statusRaw, _ := json.Marshal(est.Status)
	var status map[string]json.RawMessage
	must(t, json.Unmarshal(statusRaw, &status))
	for _, block := range []string{"cluster", "applied", "running", "externalSecrets", "drift", "health"} {
		if string(compact(t, sent[block])) != string(compact(t, status[block])) {
			t.Errorf("report.%s differs from status.%s:\n%s\n%s", block, block, sent[block], status[block])
		}
	}

	// Events on the Estate tell the story.
	var events corev1.EventList
	must(t, c.List(ctx, &events, client.InNamespace("tq-operator")))
	reasons := map[string]bool{}
	for _, e := range events.Items {
		reasons[e.Reason] = true
	}
	if !reasons["DriftDetected"] || !reasons["ReportAccepted"] {
		t.Errorf("events: %v", reasons)
	}

	// The console goes down; a new render (a changed spec) is due at once and fails harmlessly.
	fake.mu.Lock()
	fake.unavail = true
	fake.mu.Unlock()
	must(t, c.Get(ctx, types.NamespacedName{Namespace: "tq-operator", Name: "prod"}, est))
	svc := est.Spec.Services["iam"]
	svc.Version = "v0.45.5"
	est.Spec.Services["iam"] = svc
	must(t, c.Update(ctx, est))
	est = waitFor(t, c, "Reported: False/Unreachable", func(e *estatev1alpha1.Estate) bool {
		r := condition(e, controller.ConditionReported)
		return r != nil && r.Reason == "Unreachable"
	})
	if r := condition(est, controller.ConditionReported); r.Status != metav1.ConditionFalse || !strings.Contains(r.Message, "next attempt at") {
		t.Errorf("Reported = %+v", r)
	}
	if est.Status.LastReport.Outcome != "unreachable" || est.Status.LastReport.Status != 503 {
		t.Errorf("lastReport = %+v", est.Status.LastReport)
	}
	if r := condition(est, controller.ConditionObserved); r.Status != metav1.ConditionTrue || r.ObservedGeneration != est.Generation {
		t.Errorf("an unreachable console is not a reconcile failure: Observed = %+v (generation %d)", r, est.Generation)
	}
	neverGatesFlux(t, c)
	if len(est.Status.Running.Workloads) != 6 {
		t.Error("status must still be written while the console is down")
	}
}

func compact(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var v any
	must(t, json.Unmarshal(raw, &v))
	out, _ := json.Marshal(v)
	return out
}

// The spec `tq` renders is accepted by the API server as it is — capabilities: [],
// an explicit false, an omitted backup — and reads back unchanged (no default fills a field the
// render wrote, no pruning drops one).
func TestRenderedSpecRoundTripsThroughTheAPIServer(t *testing.T) {
	_, c, _ := startEnv(t)
	ctx := context.Background()
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tq-operator"}}))
	root, err := schemagen.ModuleRoot(".")
	must(t, err)
	for _, name := range []string{"rendered-spec-dev.json", "rendered-spec-prod.json"} {
		raw, err := os.ReadFile(filepath.Join(root, "api", "v1alpha1", "testdata", name))
		must(t, err)
		var spec map[string]any
		must(t, json.Unmarshal(raw, &spec))
		u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		u.SetGroupVersionKind(observe.EstateGVK)
		u.SetNamespace("tq-operator")
		u.SetName(spec["environment"].(string))
		must(t, c.Create(ctx, u))

		back := &unstructured.Unstructured{}
		back.SetGroupVersionKind(observe.EstateGVK)
		must(t, c.Get(ctx, types.NamespacedName{Namespace: "tq-operator", Name: u.GetName()}, back))
		got, _ := json.Marshal(back.Object["spec"])
		want, _ := json.Marshal(spec)
		if string(got) != string(want) {
			t.Errorf("%s: the API server changed the spec:\n in  %s\n out %s", name, want, got)
		}
		var typed estatev1alpha1.Estate
		must(t, c.Get(ctx, types.NamespacedName{Namespace: "tq-operator", Name: u.GetName()}, &typed))
		if typed.Spec.Services["iam"].Own || !typed.Spec.Services["backend"].Own || typed.Spec.Operator.Report.Interval != "15m" {
			t.Errorf("%s: typed read %+v", name, typed.Spec)
		}
	}
}
