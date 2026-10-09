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
			description = "Read a text file and its whole-file SHA-256 revision. Binary files return metadata. Limits are explicit; range reads carry the whole snapshot revision."
			properties["start_line"] = map[string]any{"type": "integer", "minimum": 1}
			properties["lines"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}
			properties["max_bytes"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}
		case WriteName:
			description = "Conditionally replace a file inside the session workspace through the shared mutation lock. Use a workspace-relative path; absolute paths must still belong to that workspace even in Full Access. The parent directory must already exist. Supply expected revision from read for replacement, or expected:{exists:false} without a digest for creation."
			properties["expected"] = revision
			properties["content"] = text()
			required = append(required, "expected", "content")
		case EditName:
			description = "Conditionally replace exact text inside the session workspace using the whole-file revision from read. Use a workspace-relative path; Full Access does not remove this tool's workspace boundary. Multiple occurrences require replace_all:true. Stale edits never overwrite a cooperating update."
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
