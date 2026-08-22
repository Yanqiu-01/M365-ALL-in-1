package web

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

func toolFunction(name string, tools []map[string]any) map[string]any {
	_, fn := declaredTool(name, tools)
	return fn
}

// declaredTool resolves a model-supplied name to one declared by the caller.
// Exact names win. A unique case-insensitive match is accepted because some
// upstream frames normalize function names, but ambiguous spellings are never
// guessed. The returned name is always the caller-declared spelling.
func declaredTool(name string, tools []map[string]any) (string, map[string]any) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	var matchedName string
	var matched map[string]any
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		n, _ := f["name"].(string)
		n = strings.TrimSpace(n)
		if n == name {
			return n, f
		}
		if n != "" && strings.EqualFold(n, name) {
			if matched != nil && matchedName != n {
				return "", nil
			}
			matchedName, matched = n, f
		}
	}
	return matchedName, matched
}

func schemaValid(args map[string]any, fn map[string]any) error {
	params, _ := fn["parameters"].(map[string]any)
	if params == nil {
		return nil
	}
	return validateJSONSchema(args, params, "arguments")
}

func validateJSONSchema(value any, schema map[string]any, path string) error {
	if enums, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enums {
			a, _ := json.Marshal(value)
			b, _ := json.Marshal(e)
			if string(a) == string(b) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s is not an allowed enum value", path)
		}
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "object":
		m, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be object", path)
		}
		if req, ok := schema["required"].([]any); ok {
			for _, raw := range req {
				n, _ := raw.(string)
				if _, yes := m[n]; !yes {
					return fmt.Errorf("missing required argument %s", n)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
			for n := range m {
				if _, yes := props[n]; !yes {
					return fmt.Errorf("%s.%s is not allowed", path, n)
				}
			}
		}
		for n, v := range m {
			if ps, ok := props[n].(map[string]any); ok {
				if err := validateJSONSchema(v, ps, path+"."+n); err != nil {
					return err
				}
			}
		}
	case "array":
		a, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be array", path)
		}
		if item, ok := schema["items"].(map[string]any); ok {
			for i, v := range a {
				if err := validateJSONSchema(v, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be string", path)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s must be number", path)
		}
	case "integer":
		n, ok := value.(float64)
		if !ok || math.Trunc(n) != n {
			return fmt.Errorf("%s must be integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be boolean", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s must be null", path)
		}
	}
	return nil
}
