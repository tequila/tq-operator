package schemagen

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
	"unicode/utf8"
)

// Validate checks a JSON document against the subset of JSON Schema the two published schemas
// use (type, const, enum, properties, required, additionalProperties, items, maxItems,
// maxLength, pattern, format date-time). It returns one message per violation, each naming
// the JSON pointer — an empty result is a valid document.
func Validate(schema map[string]any, document []byte) ([]string, error) {
	var doc any
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil, fmt.Errorf("the document is not JSON: %w", err)
	}
	var errs []string
	validate(schema, doc, "", &errs)
	return errs, nil
}

func validate(s map[string]any, v any, path string, errs *[]string) {
	fail := func(format string, args ...any) {
		p := path
		if p == "" {
			p = "/"
		}
		*errs = append(*errs, p+": "+fmt.Sprintf(format, args...))
	}
	if t, ok := s["type"]; ok && !typeMatches(t, v) {
		fail("is %s, the schema wants %s", jsonType(v), typeString(t))
		return
	}
	if c, ok := s["const"]; ok && v != c {
		fail("is %v, the schema wants the constant %v", v, c)
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if e == v {
				found = true
				break
			}
		}
		if !found {
			fail("%v is not one of %v", v, enum)
		}
	}
	switch tv := v.(type) {
	case string:
		if limit, ok := number(s["maxLength"]); ok && utf8.RuneCountInString(tv) > limit {
			fail("is %d characters, the limit is %d", utf8.RuneCountInString(tv), limit)
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(tv) {
			fail("%q does not match %s", tv, p)
		}
		if s["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339, tv); err != nil {
				fail("%q is not an RFC 3339 timestamp", tv)
			}
		}
	case []any:
		if limit, ok := number(s["maxItems"]); ok && len(tv) > limit {
			fail("has %d items, the limit is %d", len(tv), limit)
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, item := range tv {
				validate(items, item, fmt.Sprintf("%s/%d", path, i), errs)
			}
		}
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, present := tv[r.(string)]; !present {
					fail("lacks the required key %q", r)
				}
			}
		}
		keys := make([]string, 0, len(tv))
		for k := range tv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := path + "/" + k
			if ps, ok := props[k].(map[string]any); ok {
				validate(ps, tv[k], child, errs)
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					*errs = append(*errs, child+": is not in the schema")
				}
			case map[string]any:
				validate(ap, tv[k], child, errs)
			}
		}
	}
}

func typeMatches(t any, v any) bool {
	switch tv := t.(type) {
	case string:
		return jsonTypeIs(tv, v)
	case []any:
		for _, x := range tv {
			if s, ok := x.(string); ok && jsonTypeIs(s, v) {
				return true
			}
		}
	}
	return false
}

func jsonTypeIs(t string, v any) bool {
	switch t {
	case "null":
		return v == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	}
	return false
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func number(x any) (int, bool) {
	switch n := x.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
