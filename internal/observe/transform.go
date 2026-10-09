package observe

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
)

// The transforms run before an object enters the cache: what they drop is never held in
// memory, so it can never be reported, logged or leaked. Each keeps the fields one reader
// below needs and nothing else.

// Labels and annotations the readers use. Nothing else of an object's metadata is kept.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelComponent = "app.kubernetes.io/component"
	// LabelOperationsK8s stamps an estate workload with the platform estate's release that
	// rendered it; the most common value in the platform namespace is running.estate.
	LabelOperationsK8s = "estate.tequila.dev/operations-k8s"
	AnnotationApp      = "klass8s.dev/app"
	// LabelFluxName and LabelFluxNamespace are the labels Flux's kustomize-controller stamps on
	// every object it applies: the Kustomization's name and namespace. They tie a Deployment to
	// the Kustomization that applies it.
	LabelFluxName      = "kustomize.toolkit.fluxcd.io/name"
	LabelFluxNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	// AnnotationAttachedBy marks a Kustomization a developer's inner loop suspended to run a
	// local checkout over its workloads. Only its presence is read — the value, who attached, is
	// dropped before the object enters the cache.
	AnnotationAttachedBy = "tequila.dev/attached-by"
)

func keptMeta(m metav1.ObjectMeta, labels []string, annotations []string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:            m.Name,
		Namespace:       m.Namespace,
		UID:             m.UID,
		ResourceVersion: m.ResourceVersion,
		Generation:      m.Generation,
		Labels:          pick(m.Labels, labels),
		Annotations:     pick(m.Annotations, annotations),
		OwnerReferences: m.OwnerReferences,
	}
}

// pick keeps the named keys; a nil names keeps every key.
func pick(in map[string]string, names []string) map[string]string {
	if names == nil {
		return in
	}
	var out map[string]string
	for _, k := range names {
		if v, ok := in[k]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// TransformPod keeps a Pod's labels (for selector matching), owner references, container
// images, phase and per-container image ID, readiness, restart count and waiting reason.
func TransformPod(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	out := &corev1.Pod{ObjectMeta: keptMeta(pod.ObjectMeta, nil, []string{})}
	out.OwnerReferences = pod.OwnerReferences
	for _, c := range pod.Spec.Containers {
		out.Spec.Containers = append(out.Spec.Containers, corev1.Container{Name: c.Name, Image: c.Image})
	}
	out.Status.Phase = pod.Status.Phase
	for _, cs := range pod.Status.ContainerStatuses {
		kept := corev1.ContainerStatus{Name: cs.Name, ImageID: cs.ImageID, Ready: cs.Ready, RestartCount: cs.RestartCount}
		if cs.State.Waiting != nil {
			kept.State.Waiting = &corev1.ContainerStateWaiting{Reason: cs.State.Waiting.Reason}
		}
		out.Status.ContainerStatuses = append(out.Status.ContainerStatuses, kept)
	}
	return out, nil
}

// TransformNode keeps a Node's name (in memory only — never reported), provider ID (the
// platform's hint), node info and condition types and statuses. Heartbeats are dropped, so a
// Node changes in the cache only when something about it changes.
func TransformNode(obj any) (any, error) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return obj, nil
	}
	out := &corev1.Node{ObjectMeta: keptMeta(node.ObjectMeta, []string{}, []string{})}
	out.Spec.ProviderID = node.Spec.ProviderID
	out.Status.NodeInfo = corev1.NodeSystemInfo{
		KubeletVersion:  node.Status.NodeInfo.KubeletVersion,
		Architecture:    node.Status.NodeInfo.Architecture,
		OperatingSystem: node.Status.NodeInfo.OperatingSystem,
	}
	for _, c := range node.Status.Conditions {
		out.Status.Conditions = append(out.Status.Conditions, corev1.NodeCondition{Type: c.Type, Status: c.Status})
	}
	return out, nil
}

// TransformDeployment keeps a Deployment's attribution labels, Flux's two labels, the render's
// app annotation, the selector, the replica counts and each container's name and image — never
// the pod template's env, volumes or anything else of it.
func TransformDeployment(obj any) (any, error) {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return obj, nil
	}
	out := &appsv1.Deployment{ObjectMeta: keptMeta(d.ObjectMeta,
		[]string{LabelName, LabelComponent, LabelOperationsK8s, LabelFluxName, LabelFluxNamespace}, []string{AnnotationApp})}
	out.Spec.Replicas = d.Spec.Replicas
	out.Spec.Selector = d.Spec.Selector
	for _, c := range d.Spec.Template.Spec.Containers {
		out.Spec.Template.Spec.Containers = append(out.Spec.Template.Spec.Containers, corev1.Container{Name: c.Name, Image: c.Image})
	}
	out.Status = appsv1.DeploymentStatus{
		ObservedGeneration: d.Status.ObservedGeneration,
		Replicas:           d.Status.Replicas,
		ReadyReplicas:      d.Status.ReadyReplicas,
		UpdatedReplicas:    d.Status.UpdatedReplicas,
	}
	return out, nil
}

// TransformExternalSecret keeps an ExternalSecret's name and its conditions' type, status and
// reason. The spec — remote key names, the target — is never held.
func TransformExternalSecret(obj any) (any, error) {
	return transformUnstructured(obj, nil, false)
}

// TransformKustomization keeps a Kustomization's source reference, suspension, last applied
// revision, its conditions (type, status, reason, last transition — never a message) and
// whether the attachment annotation is present (its value is dropped).
func TransformKustomization(obj any) (any, error) {
	out, err := transformUnstructured(obj, [][]string{
		{"spec", "sourceRef"}, {"spec", "suspend"}, {"status", "lastAppliedRevision"},
	}, true)
	if err != nil {
		return nil, err
	}
	in, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return out, nil
	}
	if _, present := in.GetAnnotations()[AnnotationAttachedBy]; present {
		out.(*unstructured.Unstructured).SetAnnotations(map[string]string{AnnotationAttachedBy: ""})
	}
	return out, nil
}

// TransformSource keeps a GitRepository's or OCIRepository's artifact revision and conditions.
func TransformSource(obj any) (any, error) {
	return transformUnstructured(obj, [][]string{{"status", "artifact", "revision"}}, false)
}

// TransformFluxInstance keeps a FluxInstance's conditions.
func TransformFluxInstance(obj any) (any, error) {
	return transformUnstructured(obj, nil, false)
}

func transformUnstructured(obj any, fields [][]string, keepTransitionTime bool) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	out := &unstructured.Unstructured{Object: map[string]any{}}
	out.SetAPIVersion(u.GetAPIVersion())
	out.SetKind(u.GetKind())
	out.SetName(u.GetName())
	out.SetNamespace(u.GetNamespace())
	out.SetUID(u.GetUID())
	out.SetResourceVersion(u.GetResourceVersion())
	out.SetGeneration(u.GetGeneration())
	for _, f := range fields {
		if v, found, _ := unstructured.NestedFieldNoCopy(u.Object, f...); found {
			if err := unstructured.SetNestedField(out.Object, runtime.DeepCopyJSONValue(v), f...); err != nil {
				return nil, err
			}
		}
	}
	conds, found, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if found {
		kept := make([]any, 0, len(conds))
		for _, c := range conds {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			k := map[string]any{}
			names := []string{"type", "status", "reason"}
			if keepTransitionTime {
				names = append(names, "lastTransitionTime")
			}
			for _, n := range names {
				if v, ok := cm[n]; ok {
					k[n] = v
				}
			}
			kept = append(kept, k)
		}
		if err := unstructured.SetNestedSlice(out.Object, kept, "status", "conditions"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Transforms maps every kind to its transform.
var Transforms = map[string]toolscache.TransformFunc{
	PodGVK.Kind:            TransformPod,
	NodeGVK.Kind:           TransformNode,
	DeploymentGVK.Kind:     TransformDeployment,
	ExternalSecretGVK.Kind: TransformExternalSecret,
	KustomizationGVK.Kind:  TransformKustomization,
	GitRepositoryGVK.Kind:  TransformSource,
	OCIRepositoryGVK.Kind:  TransformSource,
	FluxInstanceGVK.Kind:   TransformFluxInstance,
}
