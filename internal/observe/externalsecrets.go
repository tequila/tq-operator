package observe

import (
	"context"
	"errors"
	"sort"
)

// ExternalSecret is one ExternalSecret's readiness. Its remote key names are never read.
type ExternalSecret struct {
	Namespace string
	Name      string
	Ready     bool
	// Reason is the Ready condition's reason ("" when there is no condition yet).
	Reason string
}

func (o *Observer) observeExternalSecrets(ctx context.Context, scope Scope) ([]ExternalSecret, error) {
	if !o.Kinds.Observed(ExternalSecretGVK) {
		return nil, nil
	}
	var (
		out  []ExternalSecret
		errs []error
	)
	for _, ns := range scope.namespaces() {
		items, err := o.list(ctx, ExternalSecretGVK, ns)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, u := range items {
			status, reason, _ := readyCondition(u)
			out = append(out, ExternalSecret{Namespace: u.GetNamespace(), Name: u.GetName(), Ready: status == "True", Reason: reason})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, errors.Join(errs...)
}
