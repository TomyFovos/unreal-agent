package contextbuilder

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestEmailInstructionAndUntrackedContentsRemainPublic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "untracked-notes.txt")
	content := "Author email=developer@example.com account_id=developer; review these untracked notes."
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.user(t, "task", "Implement this change. "+string(data))
	r, err := f.b.Build()
	if err != nil || !strings.Contains(requestText(r), content) || strings.Contains(requestText(r), "REDACTED") {
		t.Fatal("email or untracked content was blocked or redacted", err)
	}
}

func TestCurrentInputRedactsCredentialsWithoutStopping(t *testing.T) {
	for _, input := range []string{"api_key=sk-synthetic-secret", "Authorization: Bearer synthetic-secret", "password=synthetic-secret", "-----BEGIN PRIVATE KEY-----\nsynthetic-secret\n-----END PRIVATE KEY-----"} {
		for _, compact := range []bool{false, true} {
			b := NewBuilder()
			if compact {
				if _, err := EnableCompaction(b, contextengine.Config{}, contextengine.Constraints{}); err != nil {
					t.Fatal(err)
				}
			}
			payload, _ := json.Marshal("Continue. " + input)
			if err := b.AddExternalInput(inbox.Input{ID: "current", Kind: inbox.InputExternal, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			r, err := b.Build()
			if err != nil {
				t.Fatal("sensitive current input must not stop the turn", err)
			}
			var text strings.Builder
			for _, item := range r.Request.Input {
				if m, ok := item.Data.(llm.Message); ok {
					text.WriteString(m.Text)
				}
			}
			if strings.Contains(text.String(), "synthetic-secret") || !strings.Contains(text.String(), "Continue.") || !strings.Contains(text.String(), "[REDACTED]") {
				t.Fatalf("credential redaction failed: compact=%v input=%q output=%q", compact, input, text.String())
			}
		}
	}
}
