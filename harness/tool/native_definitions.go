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
		description := ""
		switch name {
		case ReadName:
			description = "Read text with a whole-file SHA-256 revision, including range reads. Binary files return metadata."
			properties["start_line"] = map[string]any{"type": "integer", "minimum": 1}
			properties["lines"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}
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
			description = "Search UTF-8 text by RE2 regular expression in a directory. Glob uses slash paths and ** for recursive matching. Results and traversal are bounded."
			properties["pattern"] = text()
			properties["glob"] = text()
			properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}
			properties["max_bytes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}
			required = append(required, "pattern")
		case GlobName:
			description = "List matching regular file paths under a directory, in stable traversal order. ** matches recursive directories; symlinks are not followed."
			properties["glob"] = text()
			properties["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}
			properties["max_bytes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}
		}
		defs = append(defs, Definition{Tool: llm.Tool{Type: llm.ToolFunction, Name: name, Description: description, Parameters: map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}})
	}
	return defs
}
