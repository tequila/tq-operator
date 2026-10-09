package schemagen

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tequila/tq-operator/internal/report"
)

const (
	jsonSchemaDraft = "https://json-schema.org/draft/2020-12/schema"
	// ReportSchemaID is where the report schema is published.
	ReportSchemaID = "https://docs.tequila.dev/schemas/report.estate.v1.json"
	// EstateSchemaID is where the Estate schema is published.
	EstateSchemaID = "https://docs.tequila.dev/schemas/estate.v1alpha1.json"
)

var timeType = reflect.TypeFor[metav1.Time]()

// ReportSchema generates the report schema from report.Report.
//
// The rules: every property is required — an absent value is an explicit
// null where the type allows one; every object is closed; every string is at most
// report.MaxStringLength characters; caps and enums come from the markers.
func ReportSchema(m *Markers) (map[string]any, error) {
	g := &reportGen{markers: m}
	root, err := g.schema(reflect.TypeFor[report.Report](), "")
	if err != nil {
		return nil, err
	}
	root["$schema"] = jsonSchemaDraft
	root["$id"] = ReportSchemaID
	root["title"] = report.SchemaID
	root["description"] = "The estate report tq-operator sends to the console: Estate.status plus " +
		"the declared echo and the operator's identity. Every key is required; an absent value is null."
	return root, nil
}

type reportGen struct {
	markers *Markers
}

// schema returns the schema of t; fieldKey names the field t is the type of ("" at the root).
func (g *reportGen) schema(t reflect.Type, fieldKey string) (map[string]any, error) {
	nullable := false
	if fieldKey != "" {
		_, nullable = g.markers.value(fieldKey, "nullable")
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	s, err := g.schemaOf(t, fieldKey)
	if err != nil {
		return nil, err
	}
	if desc := g.markers.description(fieldKey); desc != "" && fieldKey != "" {
		s["description"] = desc
	}
	if nullable {
		makeNullable(s)
	}
	return s, nil
}

func (g *reportGen) schemaOf(t reflect.Type, fieldKey string) (map[string]any, error) {
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time", "maxLength": report.MaxStringLength}, nil
	}
	typeKey := ""
	if t.Name() != "" && t.PkgPath() != "" {
		typeKey = t.PkgPath() + "." + t.Name()
	}
	marker := func(name string) (string, bool) {
		if v, ok := g.markers.value(fieldKey, name); ok && fieldKey != "" {
			return v, true
		}
		if typeKey != "" {
			return g.markers.value(typeKey, name)
		}
		return "", false
	}

	switch t.Kind() {
	case reflect.String:
		s := map[string]any{"type": "string", "maxLength": report.MaxStringLength}
		if v, ok := marker("schema:const"); ok {
			s = map[string]any{"type": "string", "const": v}
		}
		if v, ok := marker("kubebuilder:validation:Enum"); ok {
			values := strings.Split(v, ";")
			enum := make([]any, len(values))
			for i, e := range values {
				enum[i] = e
			}
			s["enum"] = enum
		}
		if v, ok := marker("kubebuilder:validation:Format"); ok {
			s["format"] = v
		}
		return s, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Slice:
		s := map[string]any{"type": "array"}
		limit := -1
		if v, ok := marker("kubebuilder:validation:MaxItems"); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("%s: MaxItems %q: %w", fieldKey, v, err)
			}
			limit = n
			s["maxItems"] = n
		}
		if limit == 0 {
			return s, nil // nothing may be carried, so no item shape is published
		}
		items, err := g.schema(t.Elem(), "")
		if err != nil {
			return nil, err
		}
		s["items"] = items
		return s, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("%s: map keys must be strings", fieldKey)
		}
		values, err := g.schema(t.Elem(), "")
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": values}, nil
	case reflect.Struct:
		return g.object(t, typeKey)
	default:
		return nil, fmt.Errorf("%s: type %s has no schema mapping", fieldKey, t)
	}
}

func (g *reportGen) object(t reflect.Type, typeKey string) (map[string]any, error) {
	properties := map[string]any{}
	var required []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || name == "" {
			return nil, fmt.Errorf("%s.%s: every report field needs a JSON name", typeKey, f.Name)
		}
		if strings.Contains(opts, "omitempty") || strings.Contains(opts, "inline") {
			return nil, fmt.Errorf("%s.%s: a report field is never omitted (%q)", typeKey, f.Name, opts)
		}
		fs, err := g.schema(f.Type, typeKey+"."+f.Name)
		if err != nil {
			return nil, err
		}
		properties[name] = fs
		required = append(required, name)
	}
	sort.Strings(required)
	s := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             toAny(required),
		"additionalProperties": false,
	}
	if desc := g.markers.description(typeKey); desc != "" {
		s["description"] = desc
	}
	return s, nil
}

func makeNullable(s map[string]any) {
	switch tv := s["type"].(type) {
	case string:
		s["type"] = []any{tv, "null"}
	case []any:
		for _, x := range tv {
			if x == "null" {
				return
			}
		}
		s["type"] = append(tv, "null")
	}
	if enum, ok := s["enum"].([]any); ok {
		s["enum"] = append(enum, nil)
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
