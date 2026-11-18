package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"
)

// reportSchema describes the complete typed report, not an example with a few
// optional fields. Raw daemon receipts deliberately preserve their wire schema.
// The golden locks this public format independently of fixture measurements.
func reportSchema() map[string]any {
	schema := schemaFor(reflect.TypeFor[Report]())
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = "https://gortex.dev/bench/load/report-v1.schema.json"
	schema["title"] = "Gortex load report v1"
	return schema
}

func pairedSchema() map[string]any {
	schema := schemaFor(reflect.TypeFor[pairedReport]())
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["$id"] = "https://gortex.dev/bench/load/paired-v1.schema.json"
	schema["title"] = "Gortex coupled native request comparison v1"
	return schema
}

func schemaFor(t reflect.Type) map[string]any {
	if t == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{}
	}
	if t == reflect.TypeFor[time.Time]() {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	if t.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{schemaFor(t.Elem()), map[string]any{"type": "null"}}}
	}
	switch t.Kind() {
	case reflect.Interface:
		return map[string]any{} // canonical request arguments contain heterogeneous JSON
	case reflect.Struct:
		properties := make(map[string]any)
		required := make([]string, 0, t.NumField())
		for i := range t.NumField() {
			field := t.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if field.PkgPath != "" || name == "-" {
				continue
			}
			properties[name] = schemaFor(field.Type)
			if !strings.Contains(field.Tag.Get("json"), ",omitempty") {
				required = append(required, name)
			}
		}
		return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}, "additionalProperties": schemaFor(t.Elem())}
	case reflect.Slice:
		return map[string]any{"type": []string{"array", "null"}, "items": schemaFor(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "integer"}
	}
}
