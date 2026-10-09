package controller

import (
	"strings"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
	"github.com/tequila/tq-operator/internal/observe"
	"github.com/tequila/tq-operator/internal/report"
)

// Observed builds the observed part of an Estate's status — cluster, applied, running,
// externalSecrets, drift, preflight, health — from one observation. spec is nil when no Estate
// is declared. The same blocks are the report: what this function leaves out is never sent.
//
// ownServices false drops the tenant's own services, and every services-namespace workload no
// declared service accounts for, from running, drift and health's crash-loop names — except a
// workload running a platform image, which is Tequila's to account for and stays, undeclared.
func Observed(spec *estatev1alpha1.EstateSpec, obs *observe.Observation, scope observe.Scope, ownServices bool) estatev1alpha1.EstateStatus {
	st := estatev1alpha1.EstateStatus{Preflight: nil}
	cluster := obs.Cluster
	st.Cluster = &cluster
	st.Applied = applied(obs.Flux)

	reported := map[string]bool{}
	running := estatev1alpha1.RunningStatus{
		Estate:    estatev1alpha1.RunningEstate{OperationsK8s: obs.EstateVersion},
		Workloads: []estatev1alpha1.Workload{},
	}
	attached := attachedKustomizations(obs.Flux)
	for _, w := range obs.Workloads {
		a := attribute(spec, w, scope, ownServices)
		if !a.keep {
			continue
		}
		reported[w.Namespace+"/"+w.Name] = true
		running.Workloads = append(running.Workloads, workload(w, a, attached[w.Kustomization]))
	}
	if len(running.Workloads) > report.MaxWorkloads {
		running.Workloads = running.Workloads[:report.MaxWorkloads]
		running.Truncated = true
	}
	st.Running = &running

	ext := estatev1alpha1.ExternalSecretsStatus{NotReady: []estatev1alpha1.NotReadyExternalSecret{}}
	for _, es := range obs.ExternalSecrets {
		ext.Total++
		if es.Ready {
			ext.Ready++
			continue
		}
		if len(ext.NotReady) < report.MaxNotReadySecrets {
			reason := es.Reason
			if reason == "" {
				reason = "Unknown"
			}
			ext.NotReady = append(ext.NotReady, estatev1alpha1.NotReadyExternalSecret{Namespace: es.Namespace, Name: es.Name, Reason: reason})
		}
	}
	st.ExternalSecrets = &ext

	health := estatev1alpha1.HealthStatus{Pods: estatev1alpha1.PodHealth{
		Running: obs.Pods.Running, Pending: obs.Pods.Pending, Failed: obs.Pods.Failed, CrashLooping: []string{},
	}}
	for _, name := range obs.Pods.CrashLooping {
		ns, _, _ := strings.Cut(name, "/")
		// A Deployment is named only when it is reported; another owner only in the platform
		// namespace, or in the services namespace when the tenant's own are reported.
		if reported[name] || (!isDeploymentName(obs, name) && (ns == scope.Platform && ns != scope.Services || ownServices)) {
			if len(health.Pods.CrashLooping) < report.MaxCrashLooping {
				health.Pods.CrashLooping = append(health.Pods.CrashLooping, name)
			}
		}
	}
	st.Health = &health

	st.Drift = ComputeDrift(spec, obs, running.Workloads)
	return st
}

func isDeploymentName(obs *observe.Observation, name string) bool {
	for _, w := range obs.Workloads {
		if w.Namespace+"/"+w.Name == name {
			return true
		}
	}
	return false
}

// attribution is what attribute decided for one workload.
type attribution struct {
	// service is the declared Tequila service the workload belongs to; nil otherwise.
	service *string
	// keep is whether the workload is reported at all.
	keep bool
	// undeclared: a services-namespace workload running a platform image that no declared
	// service — by label, app annotation or image name — accounts for.
	undeclared bool
}

// attribute decides whether a workload is reported and which declared service it belongs to.
// The estate's own workloads (the platform namespace) are always reported, with no service
// unless a declared Tequila service is stamped on them.
func attribute(spec *estatev1alpha1.EstateSpec, w observe.Workload, scope observe.Scope, ownServices bool) attribution {
	var (
		name     string
		declared estatev1alpha1.EstateService
		found    bool
	)
	if spec != nil {
		for _, candidate := range []string{w.NameLabel, w.App} {
			if s, ok := spec.Services[candidate]; ok && candidate != "" {
				name, declared, found = candidate, s, true
				break
			}
		}
	}
	platform := w.Namespace == scope.Platform && scope.Platform != scope.Services
	switch {
	case found && !declared.Own:
		return attribution{service: &name, keep: true}
	case platform:
		return attribution{keep: true}
	case !found && spec != nil && runsUndeclaredPlatformImage(spec, w):
		// A platform image no declared service accounts for: Tequila's to explain, so it is
		// reported whatever ownServices says — and it is a drift.
		return attribution{keep: true, undeclared: true}
	default:
		// A tenant's own service, or a workload no declared service accounts for: its name,
		// kind, replicas and images — nothing else — and only when ownServices allows.
		return attribution{keep: ownServices}
	}
}

// runsUndeclaredPlatformImage reports whether a workload runs an image whose repository path
// is platform/<name> while no declared service's name — own or Tequila-made — matches it.
func runsUndeclaredPlatformImage(spec *estatev1alpha1.EstateSpec, w observe.Workload) bool {
	names := map[string]string{}
	for name := range spec.Services {
		names[name] = ""
	}
	undeclared := false
	for _, img := range w.Images {
		if !isPlatformImage(img.Ref) {
			continue
		}
		if imageService(img.Ref, names) != "" {
			return false
		}
		undeclared = true
	}
	return undeclared
}

// attachedKustomizations names every Kustomization a developer's checkout runs over.
func attachedKustomizations(f *observe.Flux) map[string]bool {
	out := map[string]bool{}
	if f == nil {
		return out
	}
	for _, k := range f.Kustomizations {
		if k.Attached() {
			out[k.Name] = true
		}
	}
	return out
}

func workload(w observe.Workload, a attribution, attached bool) estatev1alpha1.Workload {
	state := estatev1alpha1.WorkloadManaged
	if attached {
		state = estatev1alpha1.WorkloadAttached
	}
	out := estatev1alpha1.Workload{
		Namespace:    w.Namespace,
		Name:         w.Name,
		Kind:         "Deployment",
		Service:      a.service,
		Replicas:     estatev1alpha1.Replicas{Desired: w.Desired, Ready: w.Ready},
		Images:       []estatev1alpha1.Image{},
		State:        state,
		Undeclared:   a.undeclared,
		CrashLooping: w.CrashLooping,
		Restarts:     w.Restarts,
	}
	for _, img := range w.Images {
		if len(out.Images) == report.MaxImages {
			break
		}
		out.Images = append(out.Images, estatev1alpha1.Image{Ref: img.Ref, Digest: img.Digest, Signature: signatureUnverified})
	}
	return out
}

// signatureUnverified: rung observe reports signatures without verifying them (the keys and
// the registry egress arrive with rung verify).
const signatureUnverified = "unverified"

func applied(f *observe.Flux) *estatev1alpha1.AppliedStatus {
	if f == nil {
		return nil
	}
	key := f.Primary
	if key == "" {
		for _, k := range f.Kustomizations {
			if k.SourceKey != "" {
				key = k.SourceKey
				break
			}
		}
	}
	kind, name, ok := strings.Cut(key, "/")
	if !ok || (kind != "GitRepository" && kind != "OCIRepository") {
		return nil
	}
	out := &estatev1alpha1.AppliedStatus{
		Source:         estatev1alpha1.AppliedSource{Kind: kind, Name: name, Revision: f.Sources[key]},
		Kustomizations: []estatev1alpha1.KustomizationStatus{},
	}
	for _, k := range f.Kustomizations {
		if len(out.Kustomizations) == report.MaxKustomizations {
			break
		}
		out.Kustomizations = append(out.Kustomizations, estatev1alpha1.KustomizationStatus{
			Name:            k.Name,
			Ready:           k.ReadyStatus == "True",
			AppliedRevision: k.AppliedRevision,
			Reason:          k.Reason,
			LastReconcile:   k.LastReconcile,
			Suspended:       k.Suspended,
			Attached:        k.Attached(),
		})
	}
	return out
}
