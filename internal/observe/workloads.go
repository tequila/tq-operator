package observe

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Workload is one Deployment, with what its Pods say of their images.
type Workload struct {
	Namespace string
	Name      string
	// NameLabel is app.kubernetes.io/name; App is the render's app annotation — the two
	// attribution hints the render stamps.
	NameLabel string
	App       string
	// Kustomization is the Flux Kustomization that applied the Deployment, from Flux's own
	// labels — "" when the labels are absent or name another namespace than Flux's.
	Kustomization string
	Desired       int32
	Ready         int32
	Images        []Image
	// CrashLooping is true while a container of one of the Deployment's Pods waits in
	// CrashLoopBackOff; Restarts sums the container restart counts over those Pods.
	CrashLooping bool
	Restarts     int32
}

// Image is a container image of a workload: the template's reference and the digest a Pod
// of the workload pulled for it.
type Image struct {
	Container string
	Ref       string
	Digest    *string
}

// PodSummary counts Pods by phase and names the crash-looping ones by owner.
type PodSummary struct {
	Running, Pending, Failed int32
	// CrashLooping is "<namespace>/<owner>" — a Deployment's name, or the owning object's
	// name for Pods no observed Deployment selects; never a Pod's own name.
	CrashLooping []string
}

func (o *Observer) observeWorkloads(ctx context.Context, scope Scope) ([]Workload, PodSummary, *string, error) {
	type entry struct {
		w   Workload
		sel labels.Selector // nil when the Deployment selects nothing
	}
	var (
		entries []entry
		summary PodSummary
		errs    []error
	)
	stamps := map[string]int{}

	if o.Kinds.Observed(DeploymentGVK) {
		for _, ns := range scope.namespaces() {
			var list appsv1.DeploymentList
			if err := o.Reader.List(ctx, &list, client.InNamespace(ns)); err != nil {
				errs = append(errs, err)
				continue
			}
			for _, d := range list.Items {
				e := entry{w: Workload{
					Namespace: d.Namespace,
					Name:      d.Name,
					NameLabel: d.Labels[LabelName],
					App:       d.Annotations[AnnotationApp],
					Desired:   1,
					Ready:     d.Status.ReadyReplicas,
				}}
				if d.Labels[LabelFluxNamespace] == o.FluxNamespace {
					e.w.Kustomization = d.Labels[LabelFluxName]
				}
				if d.Spec.Replicas != nil {
					e.w.Desired = *d.Spec.Replicas
				}
				for _, c := range d.Spec.Template.Spec.Containers {
					e.w.Images = append(e.w.Images, Image{Container: c.Name, Ref: c.Image})
				}
				if sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector); err == nil && !sel.Empty() {
					e.sel = sel
				}
				if ns == scope.Platform {
					if v := d.Labels[LabelOperationsK8s]; v != "" {
						stamps[v]++
					}
				}
				entries = append(entries, e)
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].w.Namespace != entries[j].w.Namespace {
				return entries[i].w.Namespace < entries[j].w.Namespace
			}
			return entries[i].w.Name < entries[j].w.Name
		})
	}

	if o.Kinds.Observed(PodGVK) {
		crash := map[string]bool{}
		for _, ns := range scope.namespaces() {
			var pods corev1.PodList
			if err := o.Reader.List(ctx, &pods, client.InNamespace(ns)); err != nil {
				errs = append(errs, err)
				continue
			}
			for i := range pods.Items {
				pod := &pods.Items[i]
				switch pod.Status.Phase {
				case corev1.PodRunning:
					summary.Running++
				case corev1.PodPending:
					summary.Pending++
				case corev1.PodFailed:
					summary.Failed++
				}
				var owner *Workload
				for j := range entries {
					if entries[j].w.Namespace == pod.Namespace && entries[j].sel != nil && entries[j].sel.Matches(labels.Set(pod.Labels)) {
						owner = &entries[j].w
						break
					}
				}
				if owner != nil {
					recordDigests(owner, pod)
					owner.Restarts = int32(min(int64(owner.Restarts)+int64(restarts(pod)), math.MaxInt32)) //nolint:gosec // clamped on this line
				}
				if crashLooping(pod) {
					switch {
					case owner != nil:
						owner.CrashLooping = true
						crash[owner.Namespace+"/"+owner.Name] = true
					case len(pod.OwnerReferences) > 0:
						crash[pod.Namespace+"/"+pod.OwnerReferences[0].Name] = true
					}
				}
			}
		}
		for name := range crash {
			summary.CrashLooping = append(summary.CrashLooping, name)
		}
		sort.Strings(summary.CrashLooping)
	}

	workloads := make([]Workload, len(entries))
	for i, e := range entries {
		workloads[i] = e.w
	}
	return workloads, summary, mostCommon(stamps), errors.Join(errs...)
}

// mostCommon returns the most frequent value (the smallest on a tie), nil for none.
func mostCommon(counts map[string]int) *string {
	var best *string
	bestN := 0
	for v, n := range counts {
		if n > bestN || (n == bestN && best != nil && v < *best) {
			vv := v
			best, bestN = &vv, n
		}
	}
	return best
}

// recordDigests takes, for each container of the workload still without a digest, the image
// ID a Pod pulled for the same reference — a ready Pod's first.
func recordDigests(w *Workload, pod *corev1.Pod) {
	specImage := map[string]string{}
	for _, c := range pod.Spec.Containers {
		specImage[c.Name] = c.Image
	}
	for i := range w.Images {
		img := &w.Images[i]
		if img.Digest != nil {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != img.Container || specImage[cs.Name] != img.Ref {
				continue
			}
			if d := DigestOf(cs.ImageID); d != "" {
				img.Digest = &d
			}
		}
	}
}

// DigestOf extracts "sha256:…" from a container status' imageID
// (docker-pullable://repo@sha256:…, repo@sha256:…, or a bare sha256:… image ID).
func DigestOf(imageID string) string {
	if i := strings.LastIndex(imageID, "@"); i >= 0 {
		imageID = imageID[i+1:]
	}
	if strings.HasPrefix(imageID, "sha256:") && len(imageID) == len("sha256:")+64 {
		return imageID
	}
	return ""
}

// restarts sums a Pod's container restart counts, saturating at the int32 maximum.
func restarts(pod *corev1.Pod) int32 {
	var total int64
	for _, cs := range pod.Status.ContainerStatuses {
		total += int64(cs.RestartCount)
	}
	return int32(min(total, math.MaxInt32)) //nolint:gosec // clamped on this line
}

// crashLooping reports whether a container of the Pod is in a crash loop: it waits in
// CrashLoopBackOff, or it lies terminated with at least one restart behind it while the Pod is
// still Running — the kubelet will start it again. The second shape matters because a kubelet
// may report the terminated state for the whole back-off and the waiting reason only briefly, or
// not at all (observed on Kubernetes 1.37).
func crashLooping(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
		if cs.State.Terminated != nil && cs.RestartCount > 0 && pod.Status.Phase == corev1.PodRunning {
			return true
		}
	}
	return false
}
