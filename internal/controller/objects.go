package controller

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tequila/tq-operator/internal/observe"
)

func typedObject(gvk schema.GroupVersionKind) client.Object {
	switch gvk {
	case observe.NodeGVK:
		return &corev1.Node{}
	case observe.PodGVK:
		return &corev1.Pod{}
	case observe.DeploymentGVK:
		return &appsv1.Deployment{}
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}

// CacheOptions is the manager's cache: every kind is listed and watched only in the namespaces
// it is read in, through its transform; anything not listed defaults to the operator's own
// namespace; a read of a kind without an informer fails rather than starting one.
func CacheOptions(ns observe.Namespaces, kinds observe.Availability, syncPeriod *time.Duration) cache.Options {
	byObject := map[client.Object]cache.ByObject{}
	for _, gvk := range observe.AllKinds() {
		if !kinds.Observed(gvk) {
			continue
		}
		opts := cache.ByObject{Transform: observe.Transforms[gvk.Kind]}
		if namespaces := ns.Of(gvk); namespaces != nil {
			opts.Namespaces = map[string]cache.Config{}
			for _, n := range namespaces {
				opts.Namespaces[n] = cache.Config{}
			}
		}
		byObject[objectFor(gvk)] = opts
	}
	return cache.Options{
		SyncPeriod:                  syncPeriod,
		DefaultNamespaces:           map[string]cache.Config{ns.Own: {}},
		ByObject:                    byObject,
		ReaderFailOnMissingInformer: true,
	}
}
