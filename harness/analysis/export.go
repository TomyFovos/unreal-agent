package analysis

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
)

func Encode(r Report, format string) ([]byte, error) {
	switch strings.ToLower(format) {
	case "json":
		return json.Marshal(r, jsontext.WithIndent("  "))
	case "markdown":
		var b strings.Builder
		b.WriteString("# Session analysis\n\nMetadata and statistics only. JSON durations use nanoseconds.\n\n")
		for _, view := range Views[:len(Views)-1] {
			b.WriteString("## " + view + "\n\n")
			for _, line := range Lines(r, view, true) {
				b.WriteString("- " + markdownText(line) + "\n")
			}
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	default:
		return nil, errors.New("export format must be JSON or Markdown")
	}
}
func markdownText(s string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "*", "\\*", "_", "\\_", "\n", " ", "\r", " ").Replace(s)
}
