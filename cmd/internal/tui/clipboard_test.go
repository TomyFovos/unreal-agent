package tui

import (
	"encoding/base64"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// /copy puts exactly what /export writes on the clipboard: the latest response
// body or the full conversation, independent of the TUI's display window.
func TestCopyMatchesExportAndUsesOSC52(t *testing.T) {
	text := strings.Repeat("日本語 👩🏽‍💻 line\n", 300)
	f := conversationFixture("earlier response", text)
	last, err := LastResponseText(t.Context(), f, "s", throughHistory(f))
	if err != nil || last != text {
		t.Fatal("latest response differs from the canonical body", err)
	}
	all, err := ConversationText(t.Context(), f, "s", throughHistory(f))
	if err != nil {
		t.Fatal(err)
	}
	path, err := ExportConversation(t.Context(), f, "s", throughHistory(f), filepath.Join(t.TempDir(), "exports"))
	if err != nil {
		t.Fatal(err)
	}
	// Only the export timestamp can differ between two renderings.
	stamp := regexp.MustCompile(`Exported: [^\n]*`)
	if stamp.ReplaceAllString(all, "") != stamp.ReplaceAllString(exportBody(t, path), "") {
		t.Fatal("/copy all differs from /export all")
	}
	sequence := clipboardSequence(all)
	if !strings.HasPrefix(sequence, "\x1b]52;c;") || !strings.HasSuffix(sequence, "\x07") {
		t.Fatalf("not an OSC 52 clipboard write: %q", sequence[:12])
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(sequence, "\x1b]52;c;"), "\x07"))
	if err != nil || string(decoded) != all {
		t.Fatal("clipboard payload does not round-trip", err)
	}
}
