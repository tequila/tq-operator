// Package schemagen generates the two published schemas from the Go types:
//
//   - schemas/report.estate.v1.json — the estate report, by reflection over report.Report,
//     with the kubebuilder-style markers of the types' doc comments read from the source
//     (the same markers controller-gen reads for the CRD, so caps and enums have one home);
//   - schemas/estate.v1alpha1.json — the Estate CRD's openAPIV3Schema as controller-gen
//     generated it, converted to JSON Schema 2020-12.
//
// It also carries a small validator for the subset of JSON Schema the two schemas use, so the
// report's allow-list test needs no third-party dependency.
package schemagen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Markers indexes the doc comments of struct types and their fields.
type Markers struct {
	// markers by "<pkgPath>.<Type>" (type-level) and "<pkgPath>.<Type>.<Field>" (field-level).
	markers map[string][]string
	// descriptions (the doc comment without its marker lines), by the same keys.
	descriptions map[string]string
}

// LoadMarkers parses the non-test Go files of each package directory, keyed by import path.
func LoadMarkers(packages map[string]string) (*Markers, error) {
	m := &Markers{markers: map[string][]string{}, descriptions: map[string]string{}}
	paths := make([]string, 0, len(packages))
	for p := range packages {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, pkgPath := range paths {
		dir := packages[pkgPath]
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		fset := token.NewFileSet()
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", name, err)
			}
			m.indexFile(pkgPath, file)
		}
	}
	return m, nil
}

func (m *Markers) indexFile(pkgPath string, file *ast.File) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			doc := ts.Doc
			if doc == nil && len(gen.Specs) == 1 {
				doc = gen.Doc
			}
			typeKey := pkgPath + "." + ts.Name.Name
			m.add(typeKey, doc)
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					m.add(typeKey+"."+name.Name, field.Doc)
				}
			}
		}
	}
}

func (m *Markers) add(key string, doc *ast.CommentGroup) {
	if doc == nil {
		return
	}
	var text []string
	for _, c := range doc.List {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), "/*"))
		if strings.HasPrefix(line, "+") {
			m.markers[key] = append(m.markers[key], line)
			continue
		}
		if line != "" {
			text = append(text, line)
		}
	}
	if len(text) > 0 {
		m.descriptions[key] = strings.Join(text, " ")
	}
}

// value returns the value of the first marker `+<name>=<value>` under key, and whether it exists.
func (m *Markers) value(key, name string) (string, bool) {
	for _, mk := range m.markers[key] {
		if mk == "+"+name {
			return "", true
		}
		if v, ok := strings.CutPrefix(mk, "+"+name+"="); ok {
			return v, true
		}
	}
	return "", false
}

func (m *Markers) description(key string) string {
	return m.descriptions[key]
}
