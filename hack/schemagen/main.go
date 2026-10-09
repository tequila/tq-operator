// Command schemagen writes schemas/report.estate.v1.json and schemas/estate.v1alpha1.json
// from the Go types and the generated CRD. `make manifests` runs it after controller-gen.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tequila/tq-operator/internal/schemagen"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

func run() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := schemagen.ModuleRoot(wd)
	if err != nil {
		return err
	}
	reportSchema, estateSchema, err := schemagen.Generate(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, "schemas"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, schemagen.ReportSchemaPath), reportSchema, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, schemagen.EstateSchemaPath), estateSchema, 0o644)
}
