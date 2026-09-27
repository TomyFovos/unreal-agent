package tool

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const (
	ASTGrepName = "ast_grep"
	ASTEditName = "ast_edit"
)

func astDefinitions() []Definition {
	result := []Definition{}
	for _, name := range []string{ASTGrepName, ASTEditName} {
		properties := map[string]any{
			"language":  map[string]any{"type": "string", "enum": []any{"go", "javascript", "python"}},
			"query":     map[string]any{"type": "string", "description": "Tree-sitter S-expression query. Capture exactly one node per match as @match; optional named captures support rewrite templates."},
			"targets":   map[string]any{"type": "array", "minItems": 1, "maxItems": 128, "items": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "expected": map[string]any{"type": "object", "properties": map[string]any{"exists": map[string]any{"type": "boolean"}, "digest": map[string]any{"type": "string"}}, "required": []any{"exists"}, "additionalProperties": false}}, "required": []any{"path"}, "additionalProperties": false}},
			"max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": 65536},
		}
		required := []any{"language", "query", "targets"}
		description := "Query actual syntax nodes in Go, JavaScript or Python. Returns bounded matches and whole-file revisions; rejects malformed syntax and ambiguous/overlapping captures."
		if name == ASTEditName {
			properties["replacement"] = map[string]any{"type": "string", "description": "Replacement template: ${capture} inserts matched source bytes; $$ escapes a dollar. The entire replacement file must parse successfully."}
			properties["apply"] = map[string]any{"type": "boolean", "default": false}
			properties["expected_matches"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}
			required = append(required, "replacement")
			description = "Preview a structural rewrite, or apply with expected_matches and every target's expected revision from preview. All targets preflight before any change; replacement is atomic per file with reported partial failure."
		}
		result = append(result, Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: name, Description: description, Parameters: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}})
	}
	return result
}
