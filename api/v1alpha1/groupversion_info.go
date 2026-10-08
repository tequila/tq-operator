// Package v1alpha1 contains the estate.tequila.dev/v1alpha1 API: the Estate resource.
//
// The tenant's own render emits one Estate per environment from tequila.yaml; Flux applies it
// beside the manifests it describes. The operator reads spec (declared state) and writes status
// (applied and running state). Nothing else writes either.
//
// +kubebuilder:object:generate=true
// +groupName=estate.tequila.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version of the Estate API.
	GroupVersion = schema.GroupVersion{Group: "estate.tequila.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the API's types with a runtime.Scheme. The package depends on
	// apimachinery alone, so anything may import it.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the API's types to a runtime.Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Estate{}, &EstateList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
