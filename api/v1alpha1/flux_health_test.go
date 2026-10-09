package v1alpha1

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// An Estate never gates the Flux Kustomization that applies it (wait: true). Flux's health check
// holds a custom resource while a top-level status.observedGeneration differs from
// metadata.generation, or while a condition of type Ready is False. So EstateStatus carries no
// observedGeneration of its own, and the generated CRD neither stores one nor prints a Ready
// column. (That the reconciler never writes a Ready condition is held by the envtest suite.)
func TestStatusNeverGatesFlux(t *testing.T) {
	status := reflect.TypeFor[EstateStatus]()
	for i := range status.NumField() {
		field := status.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.Name == "ObservedGeneration" || strings.EqualFold(name, "observedGeneration") {
			t.Errorf("EstateStatus.%s (json %q): a top-level observedGeneration makes Flux wait for the operator", field.Name, name)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "estate.tequila.dev_estates.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name                     string `json:"name"`
				AdditionalPrinterColumns []struct {
					Name     string `json:"name"`
					JSONPath string `json:"jsonPath"`
				} `json:"additionalPrinterColumns"`
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Status struct {
								Properties map[string]any `json:"properties"`
							} `json:"status"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) == 0 {
		t.Fatal("the CRD has no versions")
	}
	for _, v := range crd.Spec.Versions {
		props := v.Schema.OpenAPIV3Schema.Properties.Status.Properties
		if _, ok := props["conditions"]; !ok {
			t.Fatalf("%s: no status.conditions in the CRD's schema — the test reads the wrong path", v.Name)
		}
		if _, ok := props["observedGeneration"]; ok {
			t.Errorf("%s: the CRD stores status.observedGeneration", v.Name)
		}
		for _, col := range v.AdditionalPrinterColumns {
			if col.Name == "Ready" || strings.Contains(col.JSONPath, `"Ready"`) {
				t.Errorf("%s: printer column %s reads a Ready condition (%s)", v.Name, col.Name, col.JSONPath)
			}
		}
	}
}
