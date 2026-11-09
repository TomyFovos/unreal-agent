package contextbuilder

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func takeRedactions(t testing.TB, b *builder) []llm.ActivityRecord {
	t.Helper()
	var records []llm.ActivityRecord
	if err := b.RecordRedactions(func(r llm.ActivityRecord) error { records = append(records, r); return nil }); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestContextRedactionActivityReasonsBytesAndNoBodies(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.user(t, "task", "Inspect synthetic sources")
	const raw = "keep useful code; token=synthetic-private"
	epochExchange(t, f, "partial", "read", `{"path":"code.go"}`, raw)
	epochExchange(t, f, "private", "read", `{"path":"/synthetic/.codex/auth.json"}`, "private source body")
	f.b.AddTypedToolResult(llm.ToolResult{CallID: "unsupported", Output: []llm.ToolResultOutput{{Kind: "unsupported", Value: "unsupported body"}}}, false)
	if _, err := f.b.Build(); err != nil {
		t.Fatal(err)
	}
	records := takeRedactions(t, f.b)
	if len(records) != 4 {
		t.Fatalf("want partial result, private call/result and non-public result, got %+v", records)
	}
	for i, want := range []struct {
		reason, call string
		before       int64
	}{
		{contextengine.PartialRedaction, "partial", int64(len(raw))},
		{contextengine.PrivateSource, "private", int64(len("read") + len(`{"path":"/synthetic/.codex/auth.json"}`))},
		{contextengine.PrivateSource, "private", int64(len("private source body"))},
		{contextengine.NonPublic, "unsupported", int64(len("unsupported body"))},
	} {
		r := records[i]
		if r.Kind != "context_redaction" || r.Redaction == nil || r.Redaction.Reason != want.reason || r.Redaction.CallID != want.call || r.Redaction.BeforeBytes != want.before || r.StartedAt.IsZero() {
			t.Fatalf("%d: %+v", i, r)
		}
		if i == 0 && r.Redaction.AfterBytes != int64(len("keep useful code; [REDACTED]")) {
			t.Fatal(r.Redaction)
		}
		if i == 1 && r.Redaction.AfterBytes != 0 {
			t.Fatal("excluded call retained bytes", r.Redaction)
		}
		if i > 1 && r.Redaction.AfterBytes == 0 {
			t.Fatal("receipt replacement bytes missing", r.Redaction)
		}
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"synthetic-private", "private source body", "unsupported body", "keep useful code", "auth.json"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("body in measurements:", secret)
		}
	}
	if _, err := f.b.Build(); err != nil {
		t.Fatal(err)
	}
	if again := takeRedactions(t, f.b); len(again) != 0 {
		t.Fatal("Build recounted old projections", again)
	}
}

func TestContextRedactionRecordsSummaryProjectionAndHeader(t *testing.T) {
	f := newContextFixture(t, "openai-codex", contextengine.Config{})
	f.b.SetSystemPrompt("required instructions token=synthetic-private")
	f.b.SetSystemPrompt("required instructions token=synthetic-private")
	f.b.ApplyContext(sessionstore.ContextRecord{Kind: "summary", Text: "keep summary token=synthetic-private"})
	epochExchange(t, f, "read", "read", `{"path":"code.go"}`, "safe result")
	for _, u := range f.b.engine.Units() {
		if r, ok := u.Item.Data.(llm.ToolResult); ok {
			r.Output = []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "keep projection token=synthetic-private"}}
			f.b.ApplyContext(sessionstore.ContextRecord{Kind: "projection", UnitID: u.ID, Items: []llm.Item{{Type: llm.ItemToolResult, Data: r}}})
		}
	}
	records := takeRedactions(t, f.b)
	if len(records) != 3 {
		t.Fatal("header, summary and projection must be counted once", records)
	}
	for _, r := range records {
		if r.Redaction.Reason != contextengine.PartialRedaction {
			t.Fatal(r)
		}
	}
}

func TestLegacyBuildRecordsRedactionOnceAndAppendFailureRetainsIt(t *testing.T) {
	b := NewBuilder().(*builder)
	b.AddToolResult("legacy", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "code token=synthetic-private"}}, false)
	for range 2 {
		if _, err := b.Build(); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.New("append failed")
	if err := b.RecordRedactions(func(llm.ActivityRecord) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	records := takeRedactions(t, b)
	if len(records) != 1 || records[0].Redaction.CallID != "legacy" {
		t.Fatal(records)
	}
	if again := takeRedactions(t, b); len(again) != 0 {
		t.Fatal(again)
	}
}
