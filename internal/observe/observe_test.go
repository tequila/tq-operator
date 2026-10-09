package observe

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestDetectPlatform(t *testing.T) {
	cases := []struct {
		server   string
		kubelets []string
		provider []string
		want     string
	}{
		{"v1.35.1-eks-3cfe0ce", nil, nil, "eks"},
		{"v1.35.1", []string{"v1.35.1-eks-113cf36"}, nil, "eks"},
		{"v1.35.1+k3s1", nil, nil, "k3s"},
		{"v1.35.1", nil, []string{"digitalocean://123"}, "doks"},
		{"v1.35.1", nil, []string{"aws:///eu-central-1a/i-0abc"}, "eks"},
		{"v1.35.1", nil, []string{"kind://docker/kind/kind-control-plane"}, "generic"},
		{"", nil, nil, "generic"},
	}
	for _, c := range cases {
		if got := DetectPlatform(c.server, c.kubelets, c.provider); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
}

// A crash loop is the kubelet's CrashLoopBackOff — or a container that lies terminated with a
// restart behind it in a Pod that still runs, which some kubelets report for the whole back-off.
func TestCrashLooping(t *testing.T) {
	status := func(phase corev1.PodPhase, cs ...corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Phase: phase, ContainerStatuses: cs}}
	}
	waiting := func(reason string) corev1.ContainerState {
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
	}
	terminated := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"waiting in CrashLoopBackOff", status(corev1.PodRunning, corev1.ContainerStatus{State: waiting("CrashLoopBackOff")}), true},
		{"waiting for the image", status(corev1.PodPending, corev1.ContainerStatus{State: waiting("ImagePullBackOff")}), false},
		{"terminated after restarts, the Pod running", status(corev1.PodRunning, corev1.ContainerStatus{State: terminated, RestartCount: 1}), true},
		{"terminated for the first time", status(corev1.PodRunning, corev1.ContainerStatus{State: terminated}), false},
		{"terminated in a Pod that is done", status(corev1.PodFailed, corev1.ContainerStatus{State: terminated, RestartCount: 5}), false},
		{"running with restarts behind it", status(corev1.PodRunning, corev1.ContainerStatus{State: running, RestartCount: 5}), false},
		{"one healthy, one looping", status(corev1.PodRunning, corev1.ContainerStatus{State: running}, corev1.ContainerStatus{State: terminated, RestartCount: 2}), true},
	}
	for _, c := range cases {
		if got := crashLooping(c.pod); got != c.want {
			t.Errorf("%s: crashLooping = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDigestOf(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	for in, want := range map[string]string{
		"docker-pullable://registry.tequila.dev/platform/iam-api@" + d: d,
		"registry.tequila.dev/platform/iam-api@" + d:                   d,
		d:                   d,
		"":                  "",
		"sha256:short":      "",
		"docker://" + d[7:]: "",
	} {
		if got := DigestOf(in); got != want {
			t.Errorf("DigestOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// The transforms keep what the readers need and drop everything else — above all the
// environment, the volumes and any Secret reference a Pod or Deployment carries.
func TestTransformsDropWhatIsNeverRead(t *testing.T) {
	replicas := int32(2)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "iam-api-abc", Namespace: "tequila", Labels: map[string]string{"app.kubernetes.io/name": "iam"},
			Annotations: map[string]string{"secret-annotation": "x"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "img:v1",
			Env:     []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "hunter2"}},
			Command: []string{"php", "--token=s3cr3t"}}},
			Volumes: []corev1.Volume{{Name: "jwt", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "iam-jwt"}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Message: "pod message", ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", ImageID: "img@sha256:x", Ready: true,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "log line"}}}, {
			Name: "sidecar", RestartCount: 3,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error", Message: "panic: the stack trace", ContainerID: "containerd://deadbeef"}}}}},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "iam-api", Namespace: "tequila",
			Labels: map[string]string{"app.kubernetes.io/name": "iam", "team-secret-label": "x",
				"kustomize.toolkit.fluxcd.io/name": "services-iam", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"},
			Annotations: map[string]string{"klass8s.dev/app": "iam", "kubectl.kubernetes.io/last-applied-configuration": "{…}"}},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: *pod.Spec.DeepCopy()}},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ip-10-0-0-1", Labels: map[string]string{"topology": "x"}},
		Spec:       corev1.NodeSpec{ProviderID: "aws:///i-1"},
		Status: corev1.NodeStatus{
			NodeInfo:  corev1.NodeSystemInfo{Architecture: "arm64", KubeletVersion: "v1.35.1", MachineID: "machine-id-value", BootID: "boot-id-value"},
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue,
				LastHeartbeatTime: metav1.Now(), Message: "kubelet is posting ready status"}},
		},
	}
	es := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
		"metadata": map[string]any{"name": "iam-app", "namespace": "tequila", "annotations": map[string]any{"a": "b"}},
		"spec":     map[string]any{"data": []any{map[string]any{"remoteRef": map[string]any{"key": "prod/iam/REMOTE_KEY"}}}},
		"status": map[string]any{"binding": map[string]any{"name": "iam-app"}, "conditions": []any{map[string]any{
			"type": "Ready", "status": "False", "reason": "SecretSyncedError", "message": "could not read REMOTE_KEY"}}},
	}}
	k := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "services-iam", "namespace": "flux-system", "annotations": map[string]any{
			"tequila.dev/attached-by": "developer@example-org", "tequila.dev/attached-from": "/home/developer/src/iam",
			"tequila.dev/attached-since": "2026-10-05T12:00:00Z", "kustomize.toolkit.fluxcd.io/reconcile": "disabled"}},
		"spec": map[string]any{"path": "./services/iam/prod", "suspend": true, "postBuild": map[string]any{"substitute": map[string]any{"X": "y"}},
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "flux-system"}},
		"status": map[string]any{"lastAppliedRevision": "main@sha1:a", "inventory": map[string]any{"entries": []any{"x"}},
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded",
				"message": "Applied revision", "lastTransitionTime": "2026-10-05T13:00:00Z"}}},
	}}

	var dump strings.Builder
	for _, tc := range []struct {
		obj any
		fn  func(any) (any, error)
	}{{pod, TransformPod}, {dep, TransformDeployment}, {node, TransformNode}, {es, TransformExternalSecret}, {k, TransformKustomization}} {
		out, err := tc.fn(tc.obj)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := yaml.Marshal(out)
		dump.Write(raw)
	}
	kept := dump.String()
	for _, never := range []string{"hunter2", "DB_PASSWORD", "s3cr3t", "iam-jwt", "secret-annotation", "pod message", "log line",
		"team-secret-label", "last-applied", "10.0.0.1", "machine-id-value", "boot-id-value", "kubelet is posting", "REMOTE_KEY",
		"binding", "postBuild", "inventory", "Applied revision", "lastHeartbeatTime: \"2",
		"developer@example-org", "/home/developer", "attached-from", "attached-since", "reconcile: disabled",
		"stack trace", "deadbeef"} {
		if strings.Contains(kept, never) {
			t.Errorf("a transform kept %q", never)
		}
	}
	for _, needed := range []string{"img:v1", "img@sha256:x", "CrashLoopBackOff", "klass8s.dev/app: iam", "arm64", "v1.35.1",
		"aws:///i-1", "SecretSyncedError", "main@sha1:a", "ReconciliationSucceeded", "2026-10-05T13:00:00Z", "flux-system",
		"kustomize.toolkit.fluxcd.io/name: services-iam", "kustomize.toolkit.fluxcd.io/namespace: flux-system",
		"tequila.dev/attached-by: \"\"", "suspend: true", "exitCode: 2", "reason: Error", "restartCount: 3"} {
		if !strings.Contains(kept, needed) {
			t.Errorf("a transform dropped %q", needed)
		}
	}
	// The attachment is read as a fact, not a value: a Kustomization without the annotation
	// keeps none.
	delete(k.Object["metadata"].(map[string]any), "annotations")
	out, err := TransformKustomization(k)
	if err != nil {
		t.Fatal(err)
	}
	if ann := out.(*unstructured.Unstructured).GetAnnotations(); len(ann) != 0 {
		t.Errorf("a Kustomization without the annotation kept %v", ann)
	}
	read := readKustomization(*out.(*unstructured.Unstructured))
	if read.AttachAnnotated || !read.Suspended || read.Attached() {
		t.Errorf("read %+v", read)
	}
}
