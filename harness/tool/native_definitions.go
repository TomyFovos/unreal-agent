package tool

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const (
	ReadName  = "read"
	WriteName = "write"
	EditName  = "edit"
	GrepName  = "grep"
	GlobName  = "glob"
)

func nativeDefinitions() []Definition {
	revision := map[string]any{"type": "object", "properties": map[string]any{"exists": map[string]any{"type": "boolean"}, "digest": map[string]any{"type": "string"}}, "required": []any{"exists"}, "additionalProperties": false}
	text := func() map[string]any { return map[string]any{"type": "string"} }
	defs := []Definition{}
	for _, name := range []string{ReadName, WriteName, EditName, GrepName, GlobName} {
		properties := map[string]any{"path": text()}
		required := []any{"path"}
		parameters := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		description := ""
		switch name {
		case ReadName:
			description = "Read text with a whole-file SHA-256 revision per file. Prefer one batched call with files and 1-based ranges over many small range reads. Use path for a single file. The batch shares max_bytes. Binary files return metadata."
			startLine := map[string]any{"type": "integer", "minimum": 1}
			lines := map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}
			properties["start_line"], properties["lines"] = startLine, lines
			lineRange := map[string]any{"type": "object", "properties": map[string]any{"start_line": startLine, "lines": lines}, "additionalProperties": false}
			files := map[string]any{"type": "object", "properties": map[string]any{"path": text(), "ranges": map[string]any{"type": "array", "items": lineRange}}, "required": []any{"path"}, "additionalProperties": false}
			properties["files"] = map[string]any{"type": "array", "minItems": 1, "items": files}
			required = nil
			parameters["oneOf"] = []any{map[string]any{"required": []any{"path"}}, map[string]any{"required": []any{"files"}}}
			properties["max_bytes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}
		case WriteName:
			description = "Replace a workspace file under the shared mutation lock, including in Full Access. Absolute paths must belong to the workspace; parent directory must exist. Use expected revision from read, or expected:{exists:false} without digest to create."
			properties["expected"] = revision
			properties["content"] = text()
			required = append(required, "expected", "content")
		case EditName:
			description = "Replace exact text in a workspace file, including in Full Access, using expected whole-file revision from read. Multiple occurrences require replace_all:true; stale edits cannot overwrite updates."
			properties["expected"] = revision
			properties["old_text"] = text()
			properties["new_text"] = text()
			properties["replace_all"] = map[string]any{"type": "boolean"}
			required = append(required, "expected", "old_text", "new_text")
		case GrepName:
			description = "Search UTF-8 text by RE2 regular expression in a directory. Prefer one call with context_lines before and after each match over many small reads. Glob uses slash paths and ** for recursive matching. Results are bounded."
			properties["pattern"] = text()
			properties["glob"] = text()
			properties["context_lines"] = map[string]any{"type": "integer", "minimum": 0}
			properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}
			properties["max_bytes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}
			required = append(required, "pattern")
		case GlobName:
			description = "List matching regular file paths under a directory, in stable traversal order. ** matches recursive directories; symlinks are not followed."
			properties["glob"] = text()
			properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}
			properties["max_bytes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}
		}
		if required != nil {
			parameters["required"] = required
		}
		defs = append(defs, Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: name, Description: description, Parameters: parameters}})
	}
	return defs
}
