package schemagen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// EstateSchema converts the Estate CRD's openAPIV3Schema (as controller-gen generated it from
// the Go types) into JSON Schema 2020-12: `nullable: true` becomes a "null" type, the
// x-kubernetes-* extensions go, and every object that names its properties is closed — the
// API server prunes unknown fields silently; a validator reading this schema refuses them.
func EstateSchema(crdYAML []byte) (map[string]any, error) {
	var crd map[string]any
	if err := yaml.Unmarshal(crdYAML, &crd); err != nil {
		return nil, fmt.Errorf("parse the CRD: %w", err)
	}
	spec, _ := crd["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	for _, v := range versions {
		version, _ := v.(map[string]any)
		if version["name"] != "v1alpha1" {
			continue
		}
		s, _ := version["schema"].(map[string]any)
		openAPI, ok := s["openAPIV3Schema"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the CRD's v1alpha1 has no openAPIV3Schema")
		}
		out, _ := convertOpenAPI(openAPI).(map[string]any)
		out["$schema"] = jsonSchemaDraft
		out["$id"] = EstateSchemaID
		out["title"] = "estate.tequila.dev/v1alpha1 Estate"
		return out, nil
	}
	return nil, fmt.Errorf("the CRD serves no v1alpha1")
}

func convertOpenAPI(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := map[string]any{}
		preserveUnknown := n["x-kubernetes-preserve-unknown-fields"] == true
		for k, v := range n {
			if strings.HasPrefix(k, "x-kubernetes-") || k == "nullable" {
				continue
			}
			switch k {
			case "properties":
				props := map[string]any{}
				for name, p := range v.(map[string]any) {
					props[name] = convertOpenAPI(p)
				}
				out[k] = props
			case "items", "additionalProperties":
				out[k] = convertOpenAPI(v)
			default:
				out[k] = v
			}
		}
		if _, hasProps := n["properties"]; hasProps && !preserveUnknown {
			if _, explicit := n["additionalProperties"]; !explicit {
				out["additionalProperties"] = false
			}
		}
		if n["nullable"] == true {
			makeNullable(out)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = convertOpenAPI(v)
		}
		return out
	default:
		return node
	}
}

// Marshal renders a schema deterministically: keys sorted, two-space indent, trailing newline,
// no HTML escaping.
func Marshal(schema map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(schema); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Shape lists a schema one line per path — "<json pointer> <type>[ <annotation>…]" — so two
// schemas can be compared by structure alone (descriptions are not shape). A property its
// parent does not require carries the annotation "optional".
func Shape(schema map[string]any) []string {
	var lines []string
	var walk func(path string, s map[string]any, optional bool)
	walk = func(path string, s map[string]any, optional bool) {
		line := path
		if line == "" {
			line = "/"
		}
		line += " " + typeString(s["type"])
		if v, ok := s["const"]; ok {
			line += fmt.Sprintf(" const=%v", v)
		}
		if v, ok := s["enum"].([]any); ok {
			parts := make([]string, len(v))
			for i, e := range v {
				if e == nil {
					parts[i] = "null"
				} else {
					parts[i] = fmt.Sprint(e)
				}
			}
			line += " enum=" + strings.Join(parts, "|")
		}
		if v, ok := s["format"]; ok {
			line += fmt.Sprintf(" format=%v", v)
		}
		if v, ok := s["maxItems"]; ok {
			line += fmt.Sprintf(" maxItems=%v", v)
		}
		if optional {
			line += " optional"
		}
		lines = append(lines, line)
		if props, ok := s["properties"].(map[string]any); ok {
			required := map[string]bool{}
			if req, ok := s["required"].([]any); ok {
				for _, r := range req {
					required[fmt.Sprint(r)] = true
				}
			}
			names := make([]string, 0, len(props))
			for name := range props {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				walk(path+"/"+name, props[name].(map[string]any), !required[name])
			}
		}
		if items, ok := s["items"].(map[string]any); ok {
			walk(path+"/[]", items, false)
		}
		if ap, ok := s["additionalProperties"].(map[string]any); ok {
			walk(path+"/{}", ap, false)
		}
	}
	walk("", schema, false)
	return lines
}

func typeString(t any) string {
	switch tv := t.(type) {
	case string:
		return tv
	case []any:
		parts := make([]string, len(tv))
		for i, x := range tv {
			parts[i] = fmt.Sprint(x)
		}
		return strings.Join(parts, "|")
	default:
		return "?"
	}
}
