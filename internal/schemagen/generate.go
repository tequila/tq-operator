package schemagen

import (
	"fmt"
	"os"
	"path/filepath"
)

// ModulePath is the operator's Go module.
const ModulePath = "github.com/tequila/tq-operator"

// Paths of the generated artifacts, relative to the module root.
const (
	CRDPath          = "config/crd/bases/estate.tequila.dev_estates.yaml"
	ReportSchemaPath = "schemas/report.estate.v1.json"
	EstateSchemaPath = "schemas/estate.v1alpha1.json"
)

// Generate renders both published schemas from the module rooted at root.
func Generate(root string) (reportSchema, estateSchema []byte, err error) {
	markers, err := LoadMarkers(map[string]string{
		ModulePath + "/api/v1alpha1":    filepath.Join(root, "api", "v1alpha1"),
		ModulePath + "/internal/report": filepath.Join(root, "internal", "report"),
	})
	if err != nil {
		return nil, nil, err
	}
	rs, err := ReportSchema(markers)
	if err != nil {
		return nil, nil, fmt.Errorf("the report schema: %w", err)
	}
	if reportSchema, err = Marshal(rs); err != nil {
		return nil, nil, err
	}
	crd, err := os.ReadFile(filepath.Join(root, CRDPath))
	if err != nil {
		return nil, nil, fmt.Errorf("the CRD (run `make manifests`): %w", err)
	}
	es, err := EstateSchema(crd)
	if err != nil {
		return nil, nil, err
	}
	if estateSchema, err = Marshal(es); err != nil {
		return nil, nil, err
	}
	return reportSchema, estateSchema, nil
}

// ModuleRoot walks up from dir to the directory holding go.mod.
func ModuleRoot(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
