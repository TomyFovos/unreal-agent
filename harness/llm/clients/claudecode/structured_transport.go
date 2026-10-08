package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
)

// A serialization schema is a closed superset of the strict Action/Final
// contract. It is NOT an authorization or execution validator. Registry-specific
// requirements and the exclusive union remain in actionSchema.parse.
// Real T1-T7 probes verified this representation before production adoption.
// It reads the immutable, already compiled Registry schema snapshot.
func structuredTransportDocument(strict jsontext.Value) (jsontext.Value, error) {
	var root struct {
		Branches []struct {
			Properties map[string]jsontext.Value `json:"properties"`
		} `json:"oneOf"`
	}
	if json.Unmarshal(strict, &root) != nil || len(root.Branches) == 0 || len(root.Branches) > 129 {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	closed := func(properties map[string]any, required ...string) map[string]any {
		node := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) != 0 {
			node["required"] = required
		}
		return node
	}
	properties := map[string]any{
		"type":  map[string]any{"type": "string", "enum": []string{"final"}},
		"final": closed(map[string]any{"message": map[string]any{"type": "string"}}, "message"),
	}
	var names []string
	arguments := closed(map[string]any{})
	for _, branch := range root.Branches[1:] {
		var action struct {
			Properties map[string]jsontext.Value `json:"properties"`
		}
		var tool struct {
			Name string `json:"const"`
		}
		var params map[string]any
		if json.Unmarshal(branch.Properties["action"], &action) != nil ||
			json.Unmarshal(action.Properties["tool"], &tool) != nil || !toolIdentifier(tool.Name) ||
			json.Unmarshal(action.Properties["arguments"], &params) != nil {
			return nil, &Error{Code: "bridge_schema_invalid"}
		}
		projected, err := transportArgumentShape(params, 0)
		if err != nil {
			return nil, err
		}
		arguments, err = mergeTransportShape(arguments, projected)
		if err != nil {
			return nil, err
		}
		names = append(names, tool.Name)
	}
	if len(names) != 0 {
		slices.Sort(names)
		properties["type"] = map[string]any{"type": "string", "enum": []string{"final", "action"}}
		properties["action"] = closed(map[string]any{
			"id":        map[string]any{"type": "string"},
			"tool":      map[string]any{"type": "string", "enum": names},
			"arguments": arguments,
		}, "id", "tool", "arguments")
	}
	document, err := json.Marshal(closed(properties, "type"), json.Deterministic(true))
	if err != nil || len(document) > structuredResponseBytes {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	if _, err := compileStructuredDocument(document); err != nil {
		return nil, err
	}
	return document, nil
}

// Preserve types and declared property names, rather than duplicating tool
// schemas. Remove sampling-only numeric/string constraints; the authoritative
// validator still enforces the original constraints and per-tool required list.
// Unsupported schema shapes fail closed instead of opening arbitrary properties.
func transportArgumentShape(node map[string]any, depth int) (map[string]any, error) {
	invalid := func() (map[string]any, error) { return nil, &Error{Code: "bridge_schema_invalid"} }
	if depth > 32 {
		return invalid()
	}
	for _, keyword := range []string{"$ref", "$defs", "definitions", "oneOf", "anyOf", "allOf", "not", "if", "then", "else"} {
		if _, exists := node[keyword]; exists {
			return invalid()
		}
	}
	kind, ok := node["type"].(string)
	if !ok {
		return invalid()
	}
	projected := map[string]any{"type": kind}
	switch kind {
	case "object":
		props, ok := node["properties"].(map[string]any)
		if !ok {
			return invalid()
		}
		children := map[string]any{}
		for _, name := range slices.Sorted(maps.Keys(props)) {
			child, ok := props[name].(map[string]any)
			if !ok {
				return invalid()
			}
			shape, err := transportArgumentShape(child, depth+1)
			if err != nil {
				return nil, err
			}
			children[name] = shape
		}
		projected["properties"], projected["additionalProperties"] = children, false
	case "array":
		items, ok := node["items"].(map[string]any)
		if !ok {
			return invalid()
		}
		shape, err := transportArgumentShape(items, depth+1)
		if err != nil {
			return nil, err
		}
		projected["items"] = shape
	case "string", "integer", "number", "boolean":
	default:
		return invalid()
	}
	return projected, nil
}

func mergeTransportShape(a, b map[string]any) (map[string]any, error) {
	if a["type"] != b["type"] {
		return nil, &Error{Code: "bridge_schema_invalid"}
	}
	switch a["type"] {
	case "object":
		left, right := a["properties"].(map[string]any), b["properties"].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(right)) {
			if previous, exists := left[name]; exists {
				merged, err := mergeTransportShape(previous.(map[string]any), right[name].(map[string]any))
				if err != nil {
					return nil, err
				}
				left[name] = merged
			} else {
				left[name] = right[name]
			}
		}
	case "array":
		merged, err := mergeTransportShape(a["items"].(map[string]any), b["items"].(map[string]any))
		if err != nil {
			return nil, err
		}
		a["items"] = merged
	}
	return a, nil
}

// Tool-specific requirements remain public Registry data, carried as private
// protocol instructions rather than an unsupported remote oneOf. This is not a
// conversation message and is never canonical memory or execution authority.
func structuredRegistryContract(strict jsontext.Value) (string, error) {
	var root struct {
		Branches []struct {
			Properties map[string]jsontext.Value `json:"properties"`
		} `json:"oneOf"`
	}
	if json.Unmarshal(strict, &root) != nil || len(root.Branches) == 0 {
		return "", &Error{Code: "bridge_schema_invalid"}
	}
	var tools []map[string]any
	for _, branch := range root.Branches[1:] {
		var action struct {
			Properties map[string]jsontext.Value `json:"properties"`
		}
		var tool struct {
			Name        string `json:"const"`
			Description string `json:"description"`
		}
		if json.Unmarshal(branch.Properties["action"], &action) != nil || json.Unmarshal(action.Properties["tool"], &tool) != nil || !toolIdentifier(tool.Name) {
			return "", &Error{Code: "bridge_schema_invalid"}
		}
		tools = append(tools, map[string]any{"name": tool.Name, "description": tool.Description, "argumentsSchema": action.Properties["arguments"]})
	}
	encoded, err := json.Marshal(tools, json.Deterministic(true))
	if err != nil || len(encoded) > structuredResponseBytes {
		return "", &Error{Code: "bridge_schema_invalid"}
	}
	return "\n\nUnreal strict semantic contract: the transport object is only for serialization. For type=final, include final.message and omit action entirely. For type=action, include action.id, action.tool and action.arguments and omit final entirely. Do not include absent branches as null. Action id must be 1..64 ASCII letters, digits, underscore or hyphen. Use only declared arguments of the selected tool; satisfy that tool's required fields and constraints. Exactly one Action per generation. Unreal validates this contract before any permission or execution. Unreal Registry tool contracts (data):\n" + string(encoded), nil
}
