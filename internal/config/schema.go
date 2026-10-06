package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Schema returns the JSON Schema for agentgw.yaml.
func Schema() ([]byte, error) {
	s, err := schemaFor(reflect.TypeFor[Config]())
	if err != nil {
		return nil, err
	}
	s["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	s["$id"] = "https://github.com/olafkfreund/MCP-AgentGateway/schema/agentgw.schema.json"
	s["title"] = "agentgw config"
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func schemaFor(t reflect.Type) (map[string]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case reflect.TypeFor[Duration]():
		return map[string]any{"type": "string", "pattern": `^[-+]?(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$`}, nil
	case reflect.TypeFor[ByteSize]():
		return map[string]any{"oneOf": []any{
			map[string]any{"type": "integer", "minimum": 0},
			map[string]any{"type": "string", "pattern": `^\s*\+?[0-9]+(\s*(KiB|MiB|GiB|B))?\s*$`},
		}}, nil
	case reflect.TypeFor[Secret]():
		return map[string]any{"type": "string", "description": "env:NAME, file:/path, or (where allowed) a literal"}, nil
	}

	switch t.Kind() {
	case reflect.Interface:
		return map[string]any{}, nil
	case reflect.Struct:
		properties := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			child, err := schemaFor(f.Type)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", t, f.Name, err)
			}
			if doc := f.Tag.Get("doc"); doc != "" {
				child["description"] = doc
			}
			properties[name] = child
		}
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("unsupported map key %s", t.Key())
		}
		if t.Elem().Kind() == reflect.Interface {
			return map[string]any{}, nil
		}
		child, err := schemaFor(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": child}, nil
	case reflect.Slice, reflect.Array:
		child, err := schemaFor(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": child}, nil
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	default:
		return nil, fmt.Errorf("unsupported type %s", t)
	}
}
