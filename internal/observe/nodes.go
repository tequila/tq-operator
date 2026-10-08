package observe

import (
	"context"
	"math"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	estatev1alpha1 "github.com/tequila/tq-operator/api/v1alpha1"
)

// observeCluster reads the API server's version and the Nodes' versions and architectures.
// A Node's name is never part of the result.
func (o *Observer) observeCluster(ctx context.Context) (estatev1alpha1.ClusterStatus, error) {
	cs := estatev1alpha1.ClusterStatus{
		Platform: "generic",
		Nodes:    estatev1alpha1.NodesStatus{Architectures: []string{}, KubeletVersions: []string{}},
	}
	var firstErr error
	if o.ServerVersion != nil {
		v, err := o.ServerVersion(ctx)
		if err != nil {
			firstErr = err
		}
		cs.Kubernetes = v
	}
	var providerIDs []string
	if o.Kinds.Observed(NodeGVK) {
		var nodes corev1.NodeList
		if err := o.Reader.List(ctx, &nodes); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			cs.Nodes.Count = int32(min(len(nodes.Items), math.MaxInt32)) //nolint:gosec // clamped on this line
			for _, n := range nodes.Items {
				if a := n.Status.NodeInfo.Architecture; a != "" && !slices.Contains(cs.Nodes.Architectures, a) {
					cs.Nodes.Architectures = append(cs.Nodes.Architectures, a)
				}
				if v := n.Status.NodeInfo.KubeletVersion; v != "" && !slices.Contains(cs.Nodes.KubeletVersions, v) {
					cs.Nodes.KubeletVersions = append(cs.Nodes.KubeletVersions, v)
				}
				providerIDs = append(providerIDs, n.Spec.ProviderID)
			}
			slices.Sort(cs.Nodes.Architectures)
			slices.Sort(cs.Nodes.KubeletVersions)
		}
	}
	cs.Platform = DetectPlatform(cs.Kubernetes, cs.Nodes.KubeletVersions, providerIDs)
	return cs, firstErr
}

// DetectPlatform names the Kubernetes distribution from what the cluster says of itself: the
// version strings EKS and k3s stamp, and the provider IDs their clouds give Nodes.
func DetectPlatform(serverVersion string, kubeletVersions, providerIDs []string) string {
	versions := append([]string{serverVersion}, kubeletVersions...)
	for _, v := range versions {
		if strings.Contains(v, "-eks-") {
			return "eks"
		}
	}
	for _, v := range versions {
		if strings.Contains(v, "+k3s") {
			return "k3s"
		}
	}
	for _, p := range providerIDs {
		switch {
		case strings.HasPrefix(p, "aws://"):
			return "eks"
		case strings.HasPrefix(p, "digitalocean://"):
			return "doks"
		case strings.HasPrefix(p, "k3s://"):
			return "k3s"
		}
	}
	return "generic"
}
