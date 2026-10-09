package observe

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
)

// Observer reads the cluster through the manager's cache.
type Observer struct {
	// Reader is the cache-backed reader (the manager's client).
	Reader client.Reader
	// ServerVersion returns the API server's gitVersion.
	ServerVersion func(ctx context.Context) (string, error)
	// Kinds is the probe's result: only Observed kinds are read.
	Kinds Availability
	// FluxNamespace is where Flux's objects live.
	FluxNamespace string
}

// Scope is the estate namespaces one observation covers.
type Scope struct {
	Platform string
	Services string
}

func (s Scope) namespaces() []string {
	if s.Platform == s.Services {
		return []string{s.Platform}
	}
	return []string{s.Platform, s.Services}
}

// Observation is one read of the cluster, before attribution, filtering and caps.
type Observation struct {
	Cluster estatev1alpha1.ClusterStatus
	// Flux is nil where no Flux source or Kustomization is observed.
	Flux *Flux
	// Workloads are the Deployments of the scope, sorted by namespace and name.
	Workloads []Workload
	// Pods is the per-phase count and the crash-looping owners (<namespace>/<owner>).
	Pods PodSummary
	// ExternalSecrets are every ExternalSecret of the scope, sorted.
	ExternalSecrets []ExternalSecret
	// EstateVersion is the platform estate's release stamped on the platform namespace's workloads.
	EstateVersion *string
	// Gaps names every read that failed this time; an empty list is a complete observation.
	Gaps []string
}

// Observe reads everything rung observe reads, in the scope. A failed read is a gap, never an
// abort: the observation that could be made is returned with the gap named.
func (o *Observer) Observe(ctx context.Context, scope Scope) *Observation {
	obs := &Observation{}
	gap := func(what string, err error) {
		obs.Gaps = append(obs.Gaps, fmt.Sprintf("%s: %s", what, shortError(err)))
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cluster, err := o.observeCluster(rctx)
	if err != nil {
		gap("cluster", err)
	}
	obs.Cluster = cluster

	if obs.Flux, err = o.observeFlux(rctx); err != nil {
		gap("flux", err)
	}
	if obs.Workloads, obs.Pods, obs.EstateVersion, err = o.observeWorkloads(rctx, scope); err != nil {
		gap("workloads", err)
	}
	if obs.ExternalSecrets, err = o.observeExternalSecrets(rctx, scope); err != nil {
		gap("externalsecrets", err)
	}
	return obs
}
