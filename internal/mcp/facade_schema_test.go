package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFacadeCapabilityRequestShapeIsDirectCallerInput(t *testing.T) {
	srv, _ := setupTestServer(t)
	for _, test := range []struct {
		domain    string
		operation string
	}{
		{domain: "search", operation: "symbols"},
		{domain: "read", operation: "file"},
	} {
		t.Run(test.domain+"."+test.operation, func(t *testing.T) {
			spec, ok := srv.facades.operation(test.domain, test.operation)
			require.True(t, ok)
			capability := srv.facadeCapability(spec, true)
			requestShape := capability["request_shape"].(map[string]any)
			publicSchema := facadeSchemaMapForTest(t, capability["input_schema"])

			require.Equal(t, test.operation, requestShape["operation"])
			require.NotContains(t, requestShape, "tool")
			require.NotContains(t, requestShape, "arguments")
			require.NoError(t, validateFacadeSchema(publicSchema, requestShape, "$"))
			require.Contains(t, capability["request_shape_note"], "directly")
		})
	}
}

func TestFacadeInvalidArgumentsEnvelopeErrorIsSelfCorrecting(t *testing.T) {
	srv, _ := setupTestServer(t)
	for _, test := range []struct {
		facade       string
		operation    string
		arguments    any
		receivedType string
	}{
		{facade: "search", operation: "symbols", arguments: `{"query":"MyType"}`, receivedType: "string"},
		{facade: "read", operation: "file", arguments: []any{"main.go"}, receivedType: "array"},
		{facade: "change", operation: "impact", arguments: float64(1), receivedType: "number"},
	} {
		t.Run(test.facade+"."+test.operation, func(t *testing.T) {
			spec, ok := srv.facades.operation(test.facade, test.operation)
			require.True(t, ok)
			acceptedFields := srv.facadeAcceptedTopLevelFields(spec)

			result := srv.validateFacadeInput(spec, map[string]any{
				"operation": test.operation,
				"query":     "MyType",
				"arguments": test.arguments,
			})
			require.NotNil(t, result)
			message := toolResultText(result)
			require.Contains(t, message, `unexpected top-level key`)
			require.Contains(t, message, `received `+test.receivedType)
			require.Contains(t, message, `pass `+strings.Join(acceptedFields, "/")+` at the top level`)
			require.NotContains(t, message, "interface {}")
			require.NotContains(t, message, "float64")
		})
	}
}

func TestFacadeCapabilitiesPublicSchemasMatchEveryAvailableOperation(t *testing.T) {
	srv, _ := setupTestServer(t)
	checked := 0
	for _, domain := range facadeToolNames() {
		if domain == "capabilities" || domain == "analyze" {
			continue
		}
		staticSchema := facadeSchemaMapForTest(t, facadeToolDefinition(domain).InputSchema)
		for _, spec := range srv.capabilityOperations(domain) {
			if spec.Hidden || domain == "session" && (spec.Operation == "subscribe" || spec.Operation == "unsubscribe") {
				continue
			}
			legacy, available := srv.facades.legacy(spec.Legacy)
			if !available {
				continue
			}
			checked++
			name := domain + "." + spec.Operation
			t.Run(name, func(t *testing.T) {
				capability := srv.facadeCapability(spec, true)
				publicSchema := facadeSchemaMapForTest(t, capability["input_schema"])
				arguments := capability["request_shape"].(map[string]any)

				require.NoError(t, validateFacadeSchema(publicSchema, arguments, "$"), "public schema must accept its own request_shape")
				require.NoError(t, validateFacadeSchema(staticSchema, arguments, "$"), "request_shape must remain valid against the static tools/list schema")
				assertFacadeSchemaWithinStatic(t, publicSchema, staticSchema, "$")

				for field := range spec.Fixed {
					require.False(t, facadeSchemaDeclaresProperty(publicSchema, field), "fixed field %q leaked into public input_schema", field)
				}
				for _, field := range legacy.tool.InputSchema.Required {
					if _, fixed := spec.Fixed[field]; fixed {
						continue
					}
					require.True(t, facadeRequiredFieldCovered(spec, publicSchema, arguments, field), "required legacy field %q lost its public requirement", field)
					if facadeSelectorInShapeLowersField(spec, arguments, field) {
						require.False(t, facadeSchemaRequiresOutsideSelector(publicSchema, field), "required selector implementation field %q leaked outside target/to", field)
					}
				}
			})
		}
	}
	require.Greater(t, checked, 100, "the exhaustive contract test must exercise the registered facade catalog")
}

func facadeSchemaMapForTest(t *testing.T, raw any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(raw)
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(encoded, &schema))
	return schema
}

func validateFacadeSchema(schema map[string]any, value any, path string) error {
	if constant, ok := schema["const"]; ok && !facadeJSONValuesEqual(constant, value) {
		return fmt.Errorf("%s: value %v does not match const %v", path, value, constant)
	}
	if choices, ok := schema["enum"].([]any); ok {
		matched := false
		for _, choice := range choices {
			if facadeJSONValuesEqual(choice, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value %v is not in enum", path, value)
		}
	}
	if alternatives, ok := schema["anyOf"].([]any); ok {
		for _, raw := range alternatives {
			if candidate, ok := raw.(map[string]any); ok && validateFacadeSchema(candidate, value, path) == nil {
				return nil
			}
		}
		return fmt.Errorf("%s: no anyOf branch accepts value", path)
	}
	if alternatives, ok := schema["oneOf"].([]any); ok {
		matches := 0
		for _, raw := range alternatives {
			if candidate, ok := raw.(map[string]any); ok && validateFacadeSchema(candidate, value, path) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s: expected one matching oneOf branch, got %d", path, matches)
		}
		return nil
	}

	typeName, _ := schema["type"].(string)
	switch typeName {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object, got %T", path, value)
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, field := range facadeSchemaRequiredNames(schema) {
			if _, exists := object[field]; !exists {
				return fmt.Errorf("%s: missing required property %q", path, field)
			}
		}
		if minimum, ok := schema["minProperties"].(float64); ok && float64(len(object)) < minimum {
			return fmt.Errorf("%s: has %d properties, minimum is %.0f", path, len(object), minimum)
		}
		if maximum, ok := schema["maxProperties"].(float64); ok && float64(len(object)) > maximum {
			return fmt.Errorf("%s: has %d properties, maximum is %.0f", path, len(object), maximum)
		}
		additional, constrained := schema["additionalProperties"].(bool)
		for field, child := range object {
			raw, declared := properties[field]
			if !declared {
				if constrained && !additional {
					return fmt.Errorf("%s: undeclared property %q", path, field)
				}
				continue
			}
			childSchema, ok := raw.(map[string]any)
			if ok {
				if err := validateFacadeSchema(childSchema, child, path+"."+field); err != nil {
					return err
				}
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			encoded, _ := json.Marshal(value)
			if err := json.Unmarshal(encoded, &array); err != nil {
				return fmt.Errorf("%s: expected array, got %T", path, value)
			}
		}
		if raw, ok := schema["items"].(map[string]any); ok {
			for i, child := range array {
				if err := validateFacadeSchema(raw, child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: expected string, got %T", path, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: expected boolean, got %T", path, value)
		}
	case "integer", "number":
		switch value.(type) {
		case int, int32, int64, float32, float64:
		default:
			return fmt.Errorf("%s: expected number, got %T", path, value)
		}
	}
	return nil
}

func facadeJSONValuesEqual(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func facadeSchemaRequiredNames(schema map[string]any) []string {
	var required []string
	switch fields := schema["required"].(type) {
	case []string:
		return fields
	case []any:
		for _, field := range fields {
			if name, ok := field.(string); ok {
				required = append(required, name)
			}
		}
	}
	return required
}

func assertFacadeSchemaWithinStatic(t *testing.T, public, static map[string]any, path string) {
	t.Helper()
	if publicType, ok := public["type"].(string); ok {
		if staticType, ok := static["type"].(string); ok {
			require.Equal(t, staticType, publicType, "%s changes the static value type", path)
		}
	}
	publicProperties, _ := public["properties"].(map[string]any)
	staticProperties, _ := static["properties"].(map[string]any)
	staticAdditional, _ := static["additionalProperties"].(bool)
	for field, raw := range publicProperties {
		staticRaw, declared := staticProperties[field]
		if !declared {
			require.True(t, staticAdditional, "%s.%s is absent from the static facade schema", path, field)
			continue
		}
		publicChild, publicObject := raw.(map[string]any)
		staticChild, staticObject := staticRaw.(map[string]any)
		if publicObject && staticObject {
			assertFacadeSchemaWithinStatic(t, publicChild, staticChild, path+"."+field)
		}
	}
}

func facadeSchemaDeclaresProperty(schema map[string]any, field string) bool {
	properties, _ := schema["properties"].(map[string]any)
	if _, exists := properties[field]; exists {
		return true
	}
	for _, raw := range properties {
		if child, ok := raw.(map[string]any); ok && facadeSchemaDeclaresProperty(child, field) {
			return true
		}
	}
	return false
}

func facadeRequestLeafPaths(arguments map[string]any) []facadePublicPath {
	var paths []facadePublicPath
	for field, value := range arguments {
		if field == "operation" {
			continue
		}
		if facadePublicContainer(field) {
			if nested, ok := value.(map[string]any); ok {
				for nestedField := range nested {
					paths = append(paths, facadePublicPath{container: field, field: nestedField})
				}
			}
			continue
		}
		paths = append(paths, facadePublicPath{field: field})
	}
	return paths
}

func facadeValueAtPublicPath(arguments map[string]any, path facadePublicPath) any {
	if path.container == "" {
		return arguments[path.field]
	}
	container, _ := arguments[path.container].(map[string]any)
	return container[path.field]
}

func facadeRequiredFieldCovered(spec facadeOperationSpec, schema map[string]any, arguments map[string]any, field string) bool {
	for _, path := range facadeRequestLeafPaths(arguments) {
		value := facadeValueAtPublicPath(arguments, path)
		if _, lowers := facadeProbePublicPath(spec, path, value)[field]; lowers && facadeSchemaRequiresPublicPath(schema, path) {
			return true
		}
	}
	return false
}

func facadeSchemaRequiresPublicPath(schema map[string]any, path facadePublicPath) bool {
	if path.container == "" {
		return slices.Contains(facadeSchemaRequiredNames(schema), path.field)
	}
	if !slices.Contains(facadeSchemaRequiredNames(schema), path.container) {
		return false
	}
	properties, _ := schema["properties"].(map[string]any)
	container, _ := properties[path.container].(map[string]any)
	if path.container == "target" || path.container == "to" {
		minimum, _ := container["minProperties"].(float64)
		return minimum >= 1
	}
	return slices.Contains(facadeSchemaRequiredNames(container), path.field)
}

func facadeSelectorInShapeLowersField(spec facadeOperationSpec, arguments map[string]any, field string) bool {
	for _, container := range []string{"target", "to"} {
		selectors, _ := arguments[container].(map[string]any)
		for selector, value := range selectors {
			if _, lowers := facadeProbePublicPath(spec, facadePublicPath{container: container, field: selector}, value)[field]; lowers {
				return true
			}
		}
	}
	return false
}

func facadeSchemaRequiresOutsideSelector(schema map[string]any, field string) bool {
	properties, _ := schema["properties"].(map[string]any)
	for _, required := range facadeSchemaRequiredNames(schema) {
		if required == field {
			return true
		}
	}
	for containerName, raw := range properties {
		if containerName == "target" || containerName == "to" {
			continue
		}
		container, ok := raw.(map[string]any)
		if ok && slices.Contains(facadeSchemaRequiredNames(container), field) {
			return true
		}
	}
	return false
}
