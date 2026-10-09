// Package observe reads what the cluster runs and what Flux applied — through the manager's
// cache, which holds only the fields the transforms below keep — and probes, before the
// manager starts, which of the kinds it may read are served and permitted here.
package observe

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
)

// The kinds rung observe reads — every one named in its RBAC, nothing else.
var (
	EstateGVK         = estatev1alpha1.GroupVersion.WithKind("Estate")
	NodeGVK           = schema.GroupVersionKind{Version: "v1", Kind: "Node"}
	PodGVK            = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
	DeploymentGVK     = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	ExternalSecretGVK = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
	KustomizationGVK  = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	GitRepositoryGVK  = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}
	OCIRepositoryGVK  = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "OCIRepository"}
	FluxInstanceGVK   = schema.GroupVersionKind{Group: "fluxcd.controlplane.io", Version: "v1", Kind: "FluxInstance"}
)

// Namespaces are the namespaces the operator reads.
type Namespaces struct {
	// Own is the operator's namespace, where its Estates live.
	Own string
	// Platform and Services are the environment's two namespaces.
	Platform string
	Services string
	// Flux is where Flux's objects live (flux-system in every render).
	Flux string
}

// Estate returns the deduplicated platform and services namespaces.
func (n Namespaces) Estate() []string {
	if n.Platform == n.Services {
		return []string{n.Platform}
	}
	return []string{n.Platform, n.Services}
}

// Of returns the namespaces a kind is read in; nil for a cluster-scoped kind.
func (n Namespaces) Of(gvk schema.GroupVersionKind) []string {
	switch gvk {
	case NodeGVK:
		return nil
	case EstateGVK:
		return []string{n.Own}
	case KustomizationGVK, GitRepositoryGVK, OCIRepositoryGVK, FluxInstanceGVK:
		return []string{n.Flux}
	default:
		return n.Estate()
	}
}

// AllKinds is every kind rung observe reads, in a stable order.
func AllKinds() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		EstateGVK, NodeGVK, DeploymentGVK, PodGVK, ExternalSecretGVK,
		KustomizationGVK, GitRepositoryGVK, OCIRepositoryGVK, FluxInstanceGVK,
	}
}

// Access is what the probe found for one kind.
type Access string

const (
	// Observed: served and permitted in every namespace it is read in — watched and read.
	Observed Access = "observed"
	// Absent: the API server does not serve the kind (no CRD) — read as empty.
	Absent Access = "absent"
	// Forbidden: served, but the RBAC does not permit the read somewhere — not read.
	Forbidden Access = "forbidden"
	// Failed: the probe could not tell (timeout, server error) — not read.
	Failed Access = "failed"
)

// Availability is the probe's result per kind.
type Availability map[schema.GroupVersionKind]Probed

// Probed is one kind's probe result.
type Probed struct {
	Access Access
	Detail string
}

// Observed reports whether a kind is watched and read.
func (a Availability) Observed(gvk schema.GroupVersionKind) bool {
	return a[gvk].Access == Observed
}

// Gaps lists the kinds that are not read for a reason worth stating — Forbidden and Failed
// make the observation incomplete; Absent kinds are listed too, as a fact of the cluster.
func (a Availability) Gaps() (incomplete []string, absent []string) {
	for _, gvk := range AllKinds() {
		p := a[gvk]
		name := strings.ToLower(gvk.Kind) + "s"
		switch p.Access {
		case Forbidden, Failed:
			incomplete = append(incomplete, fmt.Sprintf("%s (%s: %s)", name, p.Access, p.Detail))
		case Absent:
			absent = append(absent, name)
		}
	}
	return incomplete, absent
}

// Equal reports whether two probes found the same access for every kind.
func (a Availability) Equal(b Availability) bool {
	for _, gvk := range AllKinds() {
		if a[gvk].Access != b[gvk].Access {
			return false
		}
	}
	return true
}

// Probe asks, for every kind, whether it is served (a REST mapping exists) and whether a
// one-item list is permitted in each namespace it is read in. It runs before the manager
// starts, with the uncached client, so a kind the RBAC does not permit never becomes an
// informer that cannot sync — the operator observes less and says so, it does not stall.
func Probe(ctx context.Context, c client.Client, mapper meta.RESTMapper, ns Namespaces) Availability {
	out := Availability{}
	for _, gvk := range AllKinds() {
		out[gvk] = probeKind(ctx, c, mapper, gvk, ns.Of(gvk))
	}
	return out
}

func probeKind(ctx context.Context, c client.Client, mapper meta.RESTMapper, gvk schema.GroupVersionKind, namespaces []string) Probed {
	if _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return Probed{Access: Absent, Detail: "not served"}
		}
		return Probed{Access: Failed, Detail: shortError(err)}
	}
	scopes := namespaces
	if scopes == nil {
		scopes = []string{""}
	}
	for _, ns := range slices.Compact(slices.Sorted(slices.Values(scopes))) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		opts := []client.ListOption{client.Limit(1)}
		if ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.List(pctx, list, opts...)
		cancel()
		switch {
		case err == nil:
		case apierrors.IsForbidden(err):
			return Probed{Access: Forbidden, Detail: "list in " + nsName(ns)}
		case meta.IsNoMatchError(err) || apierrors.IsNotFound(err):
			return Probed{Access: Absent, Detail: "not served"}
		default:
			return Probed{Access: Failed, Detail: nsName(ns) + ": " + shortError(err)}
		}
	}
	return Probed{Access: Observed}
}

func nsName(ns string) string {
	if ns == "" {
		return "the cluster"
	}
	return ns
}

func shortError(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
