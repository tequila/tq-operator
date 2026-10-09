package controller

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
	"github.com/tequila/tq-operator/internal/observe"
	"github.com/tequila/tq-operator/internal/report"
)

// driftOrder is the order drift is listed in, and the precedence of the Drifted condition's
// reason when several kinds are present.
var driftOrder = []estatev1alpha1.DriftKind{
	estatev1alpha1.DriftNoEstate,
	estatev1alpha1.DriftRunningBehind,
	estatev1alpha1.DriftAppliedBehind,
	estatev1alpha1.DriftNotReady,
	estatev1alpha1.DriftSecretNotResolvable,
	estatev1alpha1.DriftUndeclared,
}

// ComputeDrift compares declared state with the observation (the drift kinds of rung
// observe). workloads are the reported workloads only, so drift never names what the report
// leaves out.
//
//   - RunningBehind: a declared Tequila service's version is not the tag of its running
//     platform/<service>-* images (digests are compared once the render pins them by digest).
//   - AppliedBehind: a Kustomization's last applied revision is not its source's artifact.
//   - NotReady: a Kustomization or FluxInstance whose Ready is False; a Deployment with fewer
//     ready replicas than desired.
//   - SecretNotResolvable: an ExternalSecret that is not ready.
//   - Undeclared: a services-namespace workload running a platform image that spec.services
//     does not list (observed: the image reference).
//   - NoEstate: no Estate is declared (spec nil).
//
// An attached workload (a developer's checkout running over the render) and the suspended,
// annotated Kustomization behind it are never drift: the Drifted condition is what it would
// be without them. A crash loop is reported on the workload and in health, not as drift: a
// crash-looping Deployment is NotReady by its replica counts already.
func ComputeDrift(spec *estatev1alpha1.EstateSpec, obs *observe.Observation, workloads []estatev1alpha1.Workload) []estatev1alpha1.Drift {
	var out []estatev1alpha1.Drift
	add := func(kind estatev1alpha1.DriftKind, subject string, declared, observed *string) {
		out = append(out, estatev1alpha1.Drift{Kind: kind, Subject: subject, Declared: declared, Observed: observed})
	}
	attached := func(w estatev1alpha1.Workload) bool { return w.State == estatev1alpha1.WorkloadAttached }

	if spec == nil {
		add(estatev1alpha1.DriftNoEstate, "estate", nil, nil)
	} else {
		services := declaredVersions(spec)
		for _, w := range workloads {
			if attached(w) {
				continue
			}
			for _, img := range w.Images {
				name := imageService(img.Ref, services)
				if name == "" {
					continue
				}
				if tag := imageTag(img.Ref); tag != "" && tag != services[name] {
					declared, observed := services[name], tag
					add(estatev1alpha1.DriftRunningBehind, w.Namespace+"/"+w.Name, &declared, &observed)
					break
				}
			}
		}
	}

	if obs.Flux != nil {
		for _, k := range obs.Flux.Kustomizations {
			if k.Attached() {
				continue
			}
			if src := obs.Flux.Sources[k.SourceKey]; src != nil && k.AppliedRevision != nil && *k.AppliedRevision != *src {
				add(estatev1alpha1.DriftAppliedBehind, k.Name, ptr(*src), ptr(*k.AppliedRevision))
			}
			if k.ReadyStatus == "False" {
				add(estatev1alpha1.DriftNotReady, k.Name, nil, ptr(orUnknown(k.Reason)))
			}
		}
		for subject, reason := range obs.Flux.InstancesNotReady {
			add(estatev1alpha1.DriftNotReady, subject, nil, ptr(orUnknown(reason)))
		}
	}
	for _, w := range workloads {
		if attached(w) {
			continue
		}
		if w.Replicas.Desired > 0 && w.Replicas.Ready < w.Replicas.Desired {
			add(estatev1alpha1.DriftNotReady, w.Namespace+"/"+w.Name,
				ptr(strconv.Itoa(int(w.Replicas.Desired))), ptr(strconv.Itoa(int(w.Replicas.Ready))))
		}
	}
	for _, es := range obs.ExternalSecrets {
		if !es.Ready {
			add(estatev1alpha1.DriftSecretNotResolvable, es.Namespace+"/"+es.Name, nil, ptr(orUnknown(es.Reason)))
		}
	}
	for _, w := range workloads {
		if !w.Undeclared || attached(w) {
			continue
		}
		observed := ""
		for _, img := range w.Images {
			if isPlatformImage(img.Ref) {
				observed = img.Ref
				break
			}
		}
		add(estatev1alpha1.DriftUndeclared, w.Namespace+"/"+w.Name, nil, ptr(observed))
	}

	rank := map[estatev1alpha1.DriftKind]int{}
	for i, k := range driftOrder {
		rank[k] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		return out[i].Subject < out[j].Subject
	})
	if len(out) > report.MaxDrift {
		out = out[:report.MaxDrift]
	}
	return out
}

// declaredVersions maps every declared Tequila service with a pin to its version.
func declaredVersions(spec *estatev1alpha1.EstateSpec) map[string]string {
	out := map[string]string{}
	for name, s := range spec.Services {
		if !s.Own && s.Version != "" {
			out[name] = s.Version
		}
	}
	return out
}

// isPlatformImage reports whether a reference names a platform image: its repository path
// ends in platform/<name>, on whichever registry — the hosted one, the installation's own
// or a local mirror — the path decides, never the host.
func isPlatformImage(ref string) bool {
	segments := strings.Split(imageRepository(ref), "/")
	return len(segments) >= 2 && segments[len(segments)-2] == "platform"
}

// imageService returns the declared service (a key of services) whose images the reference
// names — the longest name n with a repository ending in platform/n or platform/n-* — or "".
func imageService(ref string, services map[string]string) string {
	if !isPlatformImage(ref) {
		return ""
	}
	segments := strings.Split(imageRepository(ref), "/")
	last := segments[len(segments)-1]
	best := ""
	for name := range services {
		if (last == name || strings.HasPrefix(last, name+"-")) && len(name) > len(best) {
			best = name
		}
	}
	return best
}

// imageRepository strips the tag and the digest from an image reference.
func imageRepository(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// imageTag returns an image reference's tag, "" when it has none.
func imageTag(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

// summarize renders drift as the Drifted condition's reason and message.
func summarize(drift []estatev1alpha1.Drift) (reason, message string) {
	if len(drift) == 0 {
		return "InSync", "declared, applied and running state agree"
	}
	counts := map[estatev1alpha1.DriftKind]int{}
	for _, d := range drift {
		counts[d.Kind]++
	}
	var parts []string
	for _, k := range driftOrder {
		if n := counts[k]; n > 0 {
			if reason == "" {
				reason = string(k)
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	first := drift[0]
	message = fmt.Sprintf("%s; first: %s %s", strings.Join(parts, ", "), first.Kind, first.Subject)
	if first.Declared != nil || first.Observed != nil {
		message += fmt.Sprintf(" (declared %s, observed %s)", deref(first.Declared), deref(first.Observed))
	}
	return reason, message
}

func ptr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return "—"
	}
	return *s
}

func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}
