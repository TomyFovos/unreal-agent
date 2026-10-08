package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

const (
	structuredInputTransformationBytes   = structuredResponseBytes
	structuredInputTransformationEntries = 64
)

// Installed 2.1.285 copies optional message_delta.input_transformations onto
// the Messages API message. Its zFt/qFt consumers recognize an array with
// thinking_dropped {path, reason} entries for private thinking/input-binding
// bookkeeping, independently of StructuredOutput. Admit only that checked shape.
// The fixed array -> object -> scalar structure bounds containers to depth 2
// and JSON values to 1 + 64 * 4 = 257. No path/reason value is interpreted or
// retained, and no transform can become an Action, receipt or replay authority.
func validInputTransformations(raw jsontext.Value) bool {
	var entries []jsontext.Value
	if raw.Kind() != '[' || len(raw) > structuredInputTransformationBytes ||
		json.Unmarshal(raw, &entries) != nil || len(entries) > structuredInputTransformationEntries {
		return false
	}
	for _, entry := range entries {
		var fields map[string]jsontext.Value
		if entry.Kind() != '{' || json.Unmarshal(entry, &fields) != nil || len(fields) != 3 ||
			!jsonStringEquals(fields["type"], "thinking_dropped") || fields["path"].Kind() != '"' || fields["reason"].Kind() != '"' {
			return false
		}
	}
	return true
}
