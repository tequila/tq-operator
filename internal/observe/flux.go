package observe

import (
	"context"
	"errors"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Flux is what Flux applied: its sources, its Kustomizations and the FluxInstance.
type Flux struct {
	// Sources by "<Kind>/<name>": the artifact revision, nil before the first artifact.
	Sources map[string]*string
	// Kustomizations sorted by name.
	Kustomizations []Kustomization
	// Instances are the FluxInstances whose Ready condition is False: "<namespace>/<name>" → reason.
	InstancesNotReady map[string]string
	// Primary is the source most Kustomizations apply from ("<Kind>/<name>"), "" when none.
	Primary string
}

// Kustomization is one Flux Kustomization.
type Kustomization struct {
	Name            string
	SourceKey       string // "<Kind>/<name>" of spec.sourceRef in the same namespace; "" otherwise
	Suspended       bool
	AppliedRevision *string
	// ReadyStatus is the Ready condition's status: "True", "False", "Unknown" or "" (none yet).
	ReadyStatus   string
	Reason        string
	LastReconcile *metav1.Time
}

// SourceKey names a source as "<Kind>/<name>".
func SourceKey(kind, name string) string { return kind + "/" + name }

// observeFlux reads Flux's objects in its namespace; nil when Flux is not observed at all.
func (o *Observer) observeFlux(ctx context.Context) (*Flux, error) {
	f := &Flux{Sources: map[string]*string{}, InstancesNotReady: map[string]string{}}
	var errs []error
	observedAny := false

	for _, gvk := range []schema.GroupVersionKind{GitRepositoryGVK, OCIRepositoryGVK} {
		if !o.Kinds.Observed(gvk) {
			continue
		}
		observedAny = true
		items, err := o.list(ctx, gvk, o.FluxNamespace)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, u := range items {
			rev, found, _ := unstructured.NestedString(u.Object, "status", "artifact", "revision")
			var r *string
			if found && rev != "" {
				r = &rev
			}
			f.Sources[SourceKey(gvk.Kind, u.GetName())] = r
		}
	}

	if o.Kinds.Observed(KustomizationGVK) {
		observedAny = true
		items, err := o.list(ctx, KustomizationGVK, o.FluxNamespace)
		if err != nil {
			errs = append(errs, err)
		}
		for _, u := range items {
			f.Kustomizations = append(f.Kustomizations, readKustomization(u))
		}
		sort.Slice(f.Kustomizations, func(i, j int) bool { return f.Kustomizations[i].Name < f.Kustomizations[j].Name })
	}

	if o.Kinds.Observed(FluxInstanceGVK) {
		items, err := o.list(ctx, FluxInstanceGVK, o.FluxNamespace)
		if err != nil {
			errs = append(errs, err)
		}
		for _, u := range items {
			if status, reason, _ := readyCondition(u); status == "False" {
				f.InstancesNotReady[u.GetNamespace()+"/"+u.GetName()] = reason
			}
		}
	}

	if !observedAny || (len(f.Sources) == 0 && len(f.Kustomizations) == 0) {
		return nil, errors.Join(errs...)
	}
	f.Primary = primarySource(f)
	return f, errors.Join(errs...)
}

func readKustomization(u unstructured.Unstructured) Kustomization {
	k := Kustomization{Name: u.GetName()}
	kind, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "kind")
	name, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "name")
	ns, _, _ := unstructured.NestedString(u.Object, "spec", "sourceRef", "namespace")
	if kind != "" && name != "" && (ns == "" || ns == u.GetNamespace()) {
		k.SourceKey = SourceKey(kind, name)
	}
	k.Suspended, _, _ = unstructured.NestedBool(u.Object, "spec", "suspend")
	if rev, found, _ := unstructured.NestedString(u.Object, "status", "lastAppliedRevision"); found && rev != "" {
		k.AppliedRevision = &rev
	}
	var at string
	k.ReadyStatus, k.Reason, at = readyCondition(u)
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		mt := metav1.NewTime(t.UTC())
		k.LastReconcile = &mt
	}
	return k
}

// readyCondition returns the Ready condition's status, reason and last transition time.
func readyCondition(u unstructured.Unstructured) (status, reason, lastTransition string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok || cm["type"] != "Ready" {
			continue
		}
		status, _ = cm["status"].(string)
		reason, _ = cm["reason"].(string)
		lastTransition, _ = cm["lastTransitionTime"].(string)
		return status, reason, lastTransition
	}
	return "", "", ""
}

// primarySource is the source most Kustomizations apply from; ties and the no-Kustomization
// case fall to the GitRepository the Flux Operator names flux-system, then to the first name.
func primarySource(f *Flux) string {
	counts := map[string]int{}
	for _, k := range f.Kustomizations {
		if _, known := f.Sources[k.SourceKey]; known {
			counts[k.SourceKey]++
		}
	}
	keys := make([]string, 0, len(f.Sources))
	for k := range f.Sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, bestCount := "", -1
	for _, k := range keys {
		c := counts[k]
		if c > bestCount || (c == bestCount && k == SourceKey("GitRepository", "flux-system")) {
			best, bestCount = k, c
		}
	}
	return best
}

func (o *Observer) list(ctx context.Context, gvk schema.GroupVersionKind, namespace string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := o.Reader.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}
