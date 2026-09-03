package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

type facadePublicPath struct {
	container string
	field     string
}

func (p facadePublicPath) key() string {
	if p.container == "" {
		return p.field
	}
	return p.container + "." + p.field
}

func facadePublicCapabilitySchema(
	spec facadeOperationSpec,
	legacyProperties map[string]any,
	legacyRequired []string,
	requestShape map[string]any,
) map[string]any {
	definition := facadeToolDefinition(spec.Facade)
	staticProperties := definition.InputSchema.Properties
	requestArguments := requestShape

	properties := make(map[string]any)
	requiredTop := make(map[string]struct{})
	requiredNested := make(map[string]map[string]struct{})
	requestPaths := make(map[string]struct{})
	legacyPaths := make(map[string][]facadePublicPath)

	ensureContainer := func(name string) map[string]any {
		if existing, ok := properties[name].(map[string]any); ok {
			return existing
		}
		container := map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": false,
		}
		if raw, ok := staticProperties[name].(map[string]any); ok {
			if description, ok := raw["description"]; ok {
				container["description"] = description
			}
		}
		properties[name] = container
		return container
	}
	putPath := func(path facadePublicPath, schema any) {
		if path.container == "" {
			properties[path.field] = schema
			return
		}
		container := ensureContainer(path.container)
		container["properties"].(map[string]any)[path.field] = schema
	}
	addLegacyPath := func(field string, path facadePublicPath) {
		for _, existing := range legacyPaths[field] {
			if existing == path {
				return
			}
		}
		legacyPaths[field] = append(legacyPaths[field], path)
	}

	addCandidate := func(path facadePublicPath, sample any, fromRequest bool) []string {
		if fromRequest {
			requestPaths[path.key()] = struct{}{}
		}
		lowered := facadeProbePublicPath(spec, path, sample)
		mapped := make([]string, 0, len(lowered))
		for field := range lowered {
			if _, fixed := spec.Fixed[field]; fixed {
				continue
			}
			if _, captured := legacyProperties[field]; captured {
				mapped = append(mapped, field)
			}
		}
		sort.Strings(mapped)
		if !fromRequest && len(mapped) == 0 {
			return nil
		}
		var raw any
		if path.container == "" {
			raw = staticProperties[path.field]
		} else if path.container == "target" || path.container == "to" {
			raw = facadeTargetProperties()[path.field]
		} else if container, ok := staticProperties[path.container].(map[string]any); ok {
			if nested, ok := container["properties"].(map[string]any); ok {
				raw = nested[path.field]
			}
		}
		if raw == nil && len(mapped) > 0 {
			raw = legacyProperties[mapped[0]]
		}
		putPath(path, facadePublicValueSchema(sample, raw))
		for _, field := range mapped {
			addLegacyPath(field, path)
		}
		return mapped
	}

	argumentKeys := sortedFacadeMapKeys(requestArguments)
	for _, field := range argumentKeys {
		value := requestArguments[field]
		if field == "operation" {
			schema := facadePublicValueSchema(value, staticProperties[field])
			schema["const"] = spec.Operation
			schema["enum"] = []string{spec.Operation}
			properties[field] = schema
			requiredTop[field] = struct{}{}
			continue
		}
		if facadePublicContainer(field) {
			ensureContainer(field)
			if nested, ok := value.(map[string]any); ok {
				for _, nestedField := range sortedFacadeMapKeys(nested) {
					addCandidate(facadePublicPath{container: field, field: nestedField}, nested[nestedField], true)
				}
			}
			continue
		}
		addCandidate(facadePublicPath{field: field}, value, true)
	}
	// new_user_task is a facade control field, not a legacy read_file
	// argument. Keep the exact operation schema aligned with tools/list even
	// though the legacy schema and minimal request example cannot discover it.
	if spec.Facade == "read" && spec.Operation == "file" {
		addCandidate(facadePublicPath{container: "options", field: "new_user_task"}, false, true)
	}

	// Stable documented top-level aliases are available only when the actual
	// normalizer lowers them into a field captured by this operation.
	for _, field := range sortedFacadeMapKeys(staticProperties) {
		if field == "operation" || facadePublicContainer(field) {
			continue
		}
		path := facadePublicPath{field: field}
		if _, exists := properties[field]; exists {
			continue
		}
		addCandidate(path, facadeSchemaPlaceholder(field, staticProperties[field]), false)
	}

	// A singular/plural symbol pair is one public selector family. Advertise
	// the alternate only when both forms lower to the same captured field.
	if target, ok := requestArguments["target"].(map[string]any); ok {
		for from, to := range map[string]string{"symbol": "symbols", "symbols": "symbol"} {
			fromValue, selected := target[from]
			if !selected {
				continue
			}
			fromMapped := facadeCapturedProbeFields(spec, legacyProperties, facadePublicPath{container: "target", field: from}, fromValue)
			toRaw := facadeTargetProperties()[to]
			toValue := facadeSchemaPlaceholder(to, toRaw)
			toMapped := facadeCapturedProbeFields(spec, legacyProperties, facadePublicPath{container: "target", field: to}, toValue)
			if facadeStringSetsOverlap(fromMapped, toMapped) {
				addCandidate(facadePublicPath{container: "target", field: to}, toValue, false)
			}
		}
	}

	// Some public selectors are resolved against the graph before the legacy
	// handler runs, so they cannot be inferred from legacy schema fields alone.
	// Keep them in the same canonical target envelope as their static sibling.
	if target, ok := properties["target"].(map[string]any); ok {
		targetProperties, _ := target["properties"].(map[string]any)
		var dynamicSelector string
		switch spec.Facade + "." + spec.Operation {
		case "read.editing_context":
			dynamicSelector = "symbol"
		case "change.impact":
			dynamicSelector = "file"
		}
		if dynamicSelector != "" {
			raw := facadeTargetProperties()[dynamicSelector]
			targetProperties[dynamicSelector] = facadePublicValueSchema(facadeSchemaPlaceholder(dynamicSelector, raw), raw)
		}
	}

	// Every remaining captured field is still public, but only through a stable
	// envelope. Cold domains keep their operation payload under arguments;
	// common domains use semantic envelopes with options as the safe fallback.
	for _, field := range sortedFacadeMapKeys(legacyProperties) {
		if _, fixed := spec.Fixed[field]; fixed {
			continue
		}
		if len(legacyPaths[field]) > 0 {
			continue
		}
		path := facadeFallbackPublicPath(spec, field)
		putPath(path, facadePublicValueSchema(facadeSchemaPlaceholder(field, legacyProperties[field]), legacyProperties[field]))
		addLegacyPath(field, path)
	}

	markRequired := func(path facadePublicPath) {
		if path.container == "" {
			requiredTop[path.field] = struct{}{}
			return
		}
		requiredTop[path.container] = struct{}{}
		if path.container == "target" || path.container == "to" {
			container := ensureContainer(path.container)
			container["minProperties"] = 1
			container["maxProperties"] = 1
			return
		}
		if requiredNested[path.container] == nil {
			requiredNested[path.container] = make(map[string]struct{})
		}
		requiredNested[path.container][path.field] = struct{}{}
	}
	for _, field := range legacyRequired {
		if _, fixed := spec.Fixed[field]; fixed {
			continue
		}
		paths := legacyPaths[field]
		if len(paths) == 0 {
			continue
		}
		selected := paths[0]
		for _, path := range paths {
			if _, canonical := requestPaths[path.key()]; canonical {
				selected = path
				break
			}
		}
		markRequired(selected)
	}
	for _, field := range definition.InputSchema.Required {
		if _, available := properties[field]; available {
			requiredTop[field] = struct{}{}
		}
	}
	for containerName, fields := range requiredNested {
		container := ensureContainer(containerName)
		container["required"] = sortedFacadeSetKeys(fields)
	}
	for _, name := range []string{"target", "to"} {
		if container, ok := properties[name].(map[string]any); ok {
			if nested, _ := container["properties"].(map[string]any); len(nested) > 0 {
				container["minProperties"] = 1
				container["maxProperties"] = 1
			}
		}
	}

	// View selection is universal request context, so it is absent from legacy
	// handler schemas and operation examples. Publish it explicitly here just
	// as facadeToolDefinition does for the static tools/list contract.
	properties[viewArgName] = viewSelectorSchema()

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(requiredTop) > 0 {
		schema["required"] = sortedFacadeSetKeys(requiredTop)
	}
	return schema
}

func facadePublicContainer(field string) bool {
	switch field {
	case "arguments", "target", "source", "options", "output", "context", "guard", "to":
		return true
	default:
		return false
	}
}

func facadeProbePublicPath(spec facadeOperationSpec, path facadePublicPath, value any) map[string]any {
	input := map[string]any{"operation": spec.Operation}
	if path.container == "" {
		input[path.field] = value
	} else {
		input[path.container] = map[string]any{path.field: value}
	}
	return normalizeFacadeArguments(spec, input)
}

func facadeCapturedProbeFields(spec facadeOperationSpec, properties map[string]any, path facadePublicPath, value any) map[string]struct{} {
	out := make(map[string]struct{})
	for field := range facadeProbePublicPath(spec, path, value) {
		if _, fixed := spec.Fixed[field]; fixed {
			continue
		}
		if _, captured := properties[field]; captured {
			out[field] = struct{}{}
		}
	}
	return out
}

func facadeStringSetsOverlap(left, right map[string]struct{}) bool {
	for value := range left {
		if _, ok := right[value]; ok {
			return true
		}
	}
	return false
}

func facadeFallbackPublicPath(spec facadeOperationSpec, field string) facadePublicPath {
	if facadeColdDomain(spec.Facade) {
		return facadePublicPath{container: "arguments", field: field}
	}
	if spec.Facade == "change" && spec.Operation == "impact" {
		return facadePublicPath{container: "output", field: field}
	}
	switch field {
	case "format", "max_bytes", "fields", "compact", "summary_only":
		return facadePublicPath{container: "output", field: field}
	}
	if spec.Facade == "read" {
		switch field {
		case "context_lines", "max_lines", "compress_bodies":
			return facadePublicPath{container: "context", field: field}
		}
	}
	if spec.Facade == "edit" && (strings.Contains(field, "etag") || strings.Contains(field, "hash") || strings.Contains(field, "occurrence") || strings.HasPrefix(field, "expected_")) {
		return facadePublicPath{container: "guard", field: field}
	}
	if spec.Facade == "change" || spec.Facade == "review" {
		switch field {
		case "diff", "base", "head", "branch", "path", "file", "ranges", "symbols", "changes", "scope", "workspace_edit", "steps", "source", "staged":
			return facadePublicPath{container: "source", field: field}
		}
	}
	return facadePublicPath{container: "options", field: field}
}

func facadeColdDomain(facade string) bool {
	switch facade {
	case "publish_review", "pr", "recall", "remember", "workspace", "workspace_admin", "overlay", "response":
		return true
	default:
		return false
	}
}

func facadePublicValueSchema(value, raw any) map[string]any {
	if schema, ok := raw.(map[string]any); ok && facadeSchemaAcceptsPlaceholder(schema, value) {
		return cloneFacadeSchemaMap(schema)
	}
	schema := facadeSchemaForValue(value)
	if rawSchema, ok := raw.(map[string]any); ok {
		if description, ok := rawSchema["description"]; ok {
			schema["description"] = description
		}
	}
	return schema
}

func facadeSchemaAcceptsPlaceholder(schema map[string]any, value any) bool {
	if constant, ok := schema["const"]; ok && fmt.Sprint(constant) != fmt.Sprint(value) {
		return false
	}
	switch choices := schema["enum"].(type) {
	case []any:
		matched := false
		for _, choice := range choices {
			if fmt.Sprint(choice) == fmt.Sprint(value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	case []string:
		if !slices.Contains(choices, fmt.Sprint(value)) {
			return false
		}
	}
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer", "number":
		switch value.(type) {
		case int, int32, int64, float32, float64:
			return true
		default:
			return false
		}
	case "array":
		switch value.(type) {
		case []any, []string, []map[string]any:
			return true
		default:
			return false
		}
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return true
	}
}

func facadeSchemaForValue(value any) map[string]any {
	switch typed := value.(type) {
	case bool:
		return map[string]any{"type": "boolean"}
	case int, int32, int64, float32, float64:
		return map[string]any{"type": "number"}
	case []string:
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	case []any:
		items := map[string]any{}
		if len(typed) > 0 {
			items = facadeSchemaForValue(typed[0])
		}
		return map[string]any{"type": "array", "items": items}
	case []map[string]any:
		items := map[string]any{"type": "object", "additionalProperties": true}
		if len(typed) > 0 {
			items = facadeSchemaForValue(typed[0])
		}
		return map[string]any{"type": "array", "items": items}
	case map[string]any:
		properties := make(map[string]any, len(typed))
		for _, field := range sortedFacadeMapKeys(typed) {
			properties[field] = facadeSchemaForValue(typed[field])
		}
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	default:
		return map[string]any{"type": "string"}
	}
}

func cloneFacadeSchemaMap(schema map[string]any) map[string]any {
	raw, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var clone map[string]any
	if err := json.Unmarshal(raw, &clone); err != nil {
		return schema
	}
	return clone
}

func sortedFacadeMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedFacadeSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func facadeCompleteRequiredSelectors(spec facadeOperationSpec, args map[string]any, required []string) {
	staticProperties := facadeToolDefinition(spec.Facade).InputSchema.Properties
	containers := []string{"target", "to"}
	selectors := []string{"file", "symbol", "symbols", "query", "artifact", "repo"}
	for _, field := range required {
		if _, fixed := spec.Fixed[field]; fixed {
			continue
		}
		if _, satisfied := normalizeFacadeArguments(spec, args)[field]; satisfied {
			continue
		}
		for _, container := range containers {
			if _, supported := staticProperties[container]; !supported {
				continue
			}
			if existing, ok := args[container].(map[string]any); ok && len(existing) > 0 {
				continue
			}
			matched := false
			for _, selector := range selectors {
				value := facadeSchemaPlaceholder(selector, facadeTargetProperties()[selector])
				path := facadePublicPath{container: container, field: selector}
				if _, lowersRequiredField := facadeProbePublicPath(spec, path, value)[field]; !lowersRequiredField {
					continue
				}
				args[container] = map[string]any{selector: value}
				matched = true
				break
			}
			if matched {
				break
			}
		}
	}
}

func facadeSchemaPlaceholder(field string, raw any) any {
	property, _ := raw.(map[string]any)
	if constant, ok := property["const"]; ok {
		return constant
	}
	switch choices := property["enum"].(type) {
	case []any:
		if len(choices) > 0 {
			return choices[0]
		}
	case []string:
		if len(choices) > 0 {
			return choices[0]
		}
	}
	if value, ok := property["default"]; ok {
		return value
	}
	switch property["type"] {
	case "boolean":
		return false
	case "integer", "number":
		if minimum, ok := property["minimum"]; ok {
			return minimum
		}
		return 1
	case "array":
		if item, ok := property["items"]; ok {
			return []any{facadeSchemaPlaceholder(field, item)}
		}
		return []any{"<" + field + ">"}
	case "object":
		value := map[string]any{}
		properties, _ := property["properties"].(map[string]any)
		var required []string
		switch fields := property["required"].(type) {
		case []string:
			required = append(required, fields...)
		case []any:
			for _, candidate := range fields {
				if name, ok := candidate.(string); ok {
					required = append(required, name)
				}
			}
		}
		if len(required) == 0 {
			if minimum, ok := property["minProperties"].(float64); ok && minimum > 0 {
				keys := sortedFacadeMapKeys(properties)
				if len(keys) > 0 {
					required = append(required, keys[0])
				}
			}
		}
		for _, name := range required {
			value[name] = facadeSchemaPlaceholder(name, properties[name])
		}
		return value
	default:
		return "<" + field + ">"
	}
}
