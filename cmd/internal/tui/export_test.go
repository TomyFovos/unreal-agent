package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func exportBody(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func throughHistory(f *pagedTranscriptClient) sessionstore.Sequence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sessionstore.Sequence(len(f.view.History.Items))
}

var exportSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Width is measured on terminal cells, excluding the renderer's SGR styles.
func exportFrameBounds(t *testing.T, frame Frame, w, h int) {
	t.Helper()
	plain := frame
	plain.Lines = make([]string, len(frame.Lines))
	for i, row := range frame.Lines {
		plain.Lines[i] = exportSGR.ReplaceAllString(row, "")
	}
	bounds(t, plain, w, h)
}

func TestExportLastExactCanonicalBodyIndependentOfViewport(t *testing.T) {
	for name, text := range map[string]string{
		"short": "short response", "four_kib": strings.Repeat("a", 4090),
		"long":                  strings.Repeat("raw canonical line\n", 600),
		"markdown":              "# Heading\n\n**bold** [link](https://example.invalid)\n```go\n  fmt.Println(\"original\")\n```\n</script>",
		"cjk_grapheme":          strings.Repeat("日本語 👩🏽‍💻 é\n", 200),
		"beyond_display_bound":  strings.Repeat("original ", 9000),
		"beyond_old_copy_limit": strings.Repeat("line\n", (8<<20)/5+1),
	} {
		t.Run(name, func(t *testing.T) {
			f := conversationFixture("earlier response", text)
			before, _ := json.Marshal(f.view.History.Items)
			dir := filepath.Join(t.TempDir(), "exports")
			path, err := ExportLastResponse(t.Context(), f, "s", throughHistory(f), dir)
			if err != nil {
				t.Fatal(err)
			}
			if exportBody(t, path) != text {
				t.Fatal("canonical body was wrapped, clipped, or reformatted")
			}
			choices, err := ResponseChoices(t.Context(), f, "s", throughHistory(f))
			if err != nil || len(choices) != 2 || choices[0].TurnNumber != 2 || choices[1].TurnNumber != 1 {
				t.Fatal("wrong public response choices", err)
			}
			// Rendering and scrollback cannot be sources for the exported text.
			m := NewModel("s")
			if err = m.apply(f.view); err != nil {
				t.Fatal(err)
			}
			for _, theme := range []Theme{{}, {Plain: true}, {Plain: true, ASCII: true}, {NoColor: true}} {
				for _, scroll := range []int{0, 5000} {
					frame := RenderFrame(m.Snapshot(), UIState{Theme: theme, Scroll: scroll}, 44, 14)
					exportFrameBounds(t, frame, 44, 14)
					if frame.CursorY != 13 {
						t.Fatal("composer no longer bottom")
					}
				}
			}
			selected, err := ExportResponse(t.Context(), f, "s", choices[0], dir)
			if err != nil || exportBody(t, selected) != text {
				t.Fatal("rendering affected export", err)
			}
			older, err := ExportResponse(t.Context(), f, "s", choices[1], dir)
			if err != nil || exportBody(t, older) != "earlier response" {
				t.Fatal("wrong selected response", err)
			}
			after, _ := json.Marshal(f.view.History.Items)
			if string(before) != string(after) || len(f.inputs) != 0 || len(f.view.Operations) != 0 {
				t.Fatal("export mutated canonical state")
			}
		})
	}
}

func TestExportPickerExcludesNonPublicProtectedAndInternalRecords(t *testing.T) {
	f := conversationFixture("public response")
	add := func(kind sessionstore.ItemKind, data any) {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: throughHistory(f) + 1, Kind: kind, Data: data})
	}
	for i, text := range []string{"access_token=synthetic-secret", "Bearer synthetic-auth", "api_key: synthetic-key", "account_id=private-account", "api\u202ekey: synthetic-hidden"} {
		turn := session.TurnID(fmt.Sprintf("protected-%d", i))
		add(sessionstore.ItemTurn, session.Turn{ID: turn})
		add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}}})
	}
	add(sessionstore.ItemTurn, session.Turn{ID: "private"})
	add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "private", Response: llm.Response{Output: []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "private response user"}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Phase: "analysis", Text: "private analysis"}},
		{Type: llm.ItemReasoning, Data: llm.Message{Role: llm.RoleAssistant, Text: "mis-shaped hidden reasoning"}},
	}}})
	add(sessionstore.ItemTurn, session.Turn{ID: "error-only"})
	add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "error-only", Response: llm.Response{Failure: &llm.Failure{Message: "private provider failure"}}})
	add(sessionstore.ItemTurn, session.Turn{ID: "empty"})
	add(sessionstore.ItemTurn, session.Turn{ID: "compaction", Type: session.TurnCompaction})
	add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "compaction", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "internal compaction message"}}}}})
	add(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: "configuration", Configuration: []byte(`{"token":"private control"}`)})
	for _, kind := range []inbox.InputKind{inbox.InputControl, inbox.InputPeer, inbox.InputCrash} {
		add(sessionstore.ItemInput, inbox.Input{Kind: kind, Payload: []byte(`"private control peer crash"`)})
	}
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`{"private_entry":"private credential"}`)})
	add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: []byte(`"password=synthetic-password"`)})
	choices, err := ResponseChoices(t.Context(), f, "s", throughHistory(f))
	if err != nil || len(choices) != 1 || choices[0].TurnNumber != 1 || choices[0].Preview != "public response" {
		t.Fatal("non-exportable response entered picker", err, choices)
	}
	dir := filepath.Join(t.TempDir(), "exports")
	for _, choice := range choices {
		path, err := ExportResponse(t.Context(), f, "s", choice, dir)
		if err != nil || exportBody(t, path) != "public response" {
			t.Fatal("picker item not exportable", err)
		}
	}
	if _, err = ExportResponse(t.Context(), f, "s", ResponseChoice{Sequence: 5}, dir); err == nil {
		t.Fatal("protected response exported directly")
	}
	if _, err = ExportResponse(t.Context(), f, "s", ResponseChoice{Sequence: throughHistory(f) + 1}, dir); err == nil {
		t.Fatal("nonexistent response exported")
	}
	path, err := ExportConversation(t.Context(), f, "s", throughHistory(f), dir)
	if err != nil {
		t.Fatal(err)
	}
	got := exportBody(t, path)
	for _, excluded := range []string{"synthetic-", "private", "reasoning", "tool arguments", "internal compaction", "provider-state", "control peer crash"} {
		if strings.Contains(got, excluded) {
			t.Fatal("protected/internal material exported", excluded)
		}
	}
}

func TestExportLastNeverSilentlySelectsEarlierResponseWhenLatestIsProtected(t *testing.T) {
	f := conversationFixture("earlier safe response", "api_key=synthetic-protected")
	dir := filepath.Join(t.TempDir(), "exports")
	path, err := ExportLastResponse(t.Context(), f, "s", throughHistory(f), dir)
	var safe *ExportError
	if path != "" || !errors.As(err, &safe) || safe.Code != "response_not_exportable" || strings.Contains(err.Error(), "synthetic-protected") {
		t.Fatal("last silently selected an older response", path, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("refused last response created a file", err)
	}
	choices, err := ResponseChoices(t.Context(), f, "s", throughHistory(f))
	if err != nil || len(choices) != 1 || choices[0].Preview != "earlier safe response" {
		t.Fatal("safe earlier response should remain explicitly selectable", err)
	}
}

func TestExportAllCompleteCanonicalHistoryBeyondDisplayWindow(t *testing.T) {
	f := &pagedTranscriptClient{fakeClient: newFake(), pageSize: 17}
	add := func(kind sessionstore.ItemKind, data any, at time.Time) {
		f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: sessionstore.Sequence(len(f.view.History.Items) + 1), Kind: kind, Data: data, RecordedAt: at})
	}
	choice := sessionstore.RuntimeSelection{Version: 1, Revision: 1, RequestID: "runtime-one", Provider: "claude-code", Model: "sonnet", Effort: llm.ReasoningEffort("high")}
	initial := choice
	add(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &initial}, time.Time{})
	at := time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC)
	for i := range 650 { // 1950+ records and 1300 public messages, not a display page.
		payload, _ := json.Marshal(fmt.Sprintf("user-%04d 日本語", i))
		add(sessionstore.ItemInput, inbox.Input{Kind: inbox.InputExternal, Payload: payload}, at)
		turn := session.TurnID(fmt.Sprintf("turn-%04d", i))
		add(sessionstore.ItemTurn, session.Turn{ID: turn, RuntimeRevision: choice.Revision}, at)
		add(sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: turn, Response: llm.Response{Model: "claude-observed-model", Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: fmt.Sprintf("agent-%04d 日本語 👩🏽‍💻", i)}}}}}, at)
		if i == 300 {
			choice = sessionstore.RuntimeSelection{Version: 1, Revision: 2, RequestID: "runtime-two", Provider: "openai-codex", Model: "synthetic-gpt", Effort: llm.ReasoningEffort("medium")}
			copy := choice
			add(sessionstore.ItemHostRecord, sessionstore.HostRecord{Kind: sessionstore.HostRuntimeApplied, Selection: &copy}, at)
		}
	}
	before, _ := json.Marshal(f.view.History.Items)
	m := NewModel("long-session")
	if err := m.apply(f.view); err != nil {
		t.Fatal(err)
	}
	if !m.Snapshot().OlderDropped || len(m.Snapshot().Entries) > 1024 {
		t.Fatal("fixture did not exceed TUI window")
	}
	path, err := ExportConversation(t.Context(), f, "long-session", throughHistory(f), filepath.Join(t.TempDir(), "exports"))
	if err != nil {
		t.Fatal(err)
	}
	got := exportBody(t, path)
	if strings.Count(got, "### You\n") != 650 || strings.Count(got, "### Agent\n") != 650 {
		t.Fatal("full conversation truncated to display window")
	}
	position := 0
	for i := range 650 {
		for _, role := range []string{"user", "agent"} {
			needle := fmt.Sprintf("%s-%04d", role, i)
			n := strings.Index(got[position:], needle)
			if n < 0 {
				t.Fatalf("missing or reordered canonical body %s", needle)
			}
			position += n + len(needle)
		}
	}
	for _, known := range []string{"## Turn 1\n", "## Turn 650\n", "Timestamp: 2026-10-05T12:30:00Z", "Provider: claude-code", "Model: sonnet", "Effort: high", "Provider: openai-codex", "Model: synthetic-gpt", "Effort: medium", "Observed model: claude-observed-model"} {
		if !strings.Contains(got, known) {
			t.Fatal("known canonical metadata missing", known)
		}
	}
	after, _ := json.Marshal(f.view.History.Items)
	if string(before) != string(after) || len(f.inputs) != 0 || len(f.view.Operations) != 0 {
		t.Fatal("export changed canonical history/operations")
	}
	choices, err := ResponseChoices(t.Context(), f, "s", throughHistory(f))
	if err != nil || len(choices) != 650 || choices[len(choices)-1].TurnNumber != 1 {
		t.Fatal("picker lost pre-window public responses", err)
	}
}

func TestExportUnknownMetadataSafeTextAndPublicMessageParts(t *testing.T) {
	f := conversationFixture("first public part")
	response := f.view.History.Items[2].Data.(sessionstore.ModelResponse)
	legacy := response.Response.Output[1].Data.(llm.Message)
	legacy.Role = ""
	response.Response.Output[1].Data = legacy
	response.Response.Output = append(response.Response.Output, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: " \n\t "}})
	response.Response.Output = append(response.Response.Output, llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Phase: "final", Text: "second\n日本語 👩🏽‍💻\x1b\u202e\x00\n</script>"}})
	f.view.History.Items[2].Data = response
	dir := filepath.Join(t.TempDir(), "exports")
	last, err := ExportLastResponse(t.Context(), f, "s", throughHistory(f), dir)
	if err != nil || exportBody(t, last) != "first public part\n\n \n\t \n\nsecond\n日本語 👩🏽‍💻\n</script>" {
		t.Fatal("public parts/control policy changed", err)
	}
	all, err := ExportConversation(t.Context(), f, "s", throughHistory(f), dir)
	if err != nil {
		t.Fatal(err)
	}
	got := exportBody(t, all)
	for _, guessed := range []string{"Provider:", "Model:", "Effort:", "Timestamp:", "Observed model:"} {
		if strings.Contains(got, guessed) {
			t.Fatal("unknown metadata guessed", guessed)
		}
	}
}

type exportFailingReader struct {
	*pagedTranscriptClient
	calls, failAt int
}

type exportGapReader struct {
	*pagedTranscriptClient
	calls int
}

func (r *exportGapReader) Inspect(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	r.calls++
	v, err := r.pagedTranscriptClient.Inspect(ctx, id, after, limit)
	if r.calls == 5 && err == nil {
		// Jump past the captured prefix during the write pass. A page that
		// omits required canonical records cannot be treated as completion.
		v.History.Items = []host.HistoryItem{{Sequence: 7}}
		v.History.More = false
	}
	return v, err
}

func TestExportHistoryGapPastSnapshotBoundaryCannotPublishPartialFile(t *testing.T) {
	f := conversationFixture("first", "second")
	r := &exportGapReader{pagedTranscriptClient: f}
	dir := filepath.Join(t.TempDir(), "exports")
	path, err := ExportConversation(t.Context(), r, "s", throughHistory(f), dir)
	var safe *ExportError
	if path != "" || !errors.As(err, &safe) || safe.Code != "history_unavailable" {
		t.Fatal("incomplete canonical prefix exported as a completed file", path, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatal("incomplete history left a partial export", err)
	}
}

func (r *exportFailingReader) Inspect(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	r.calls++
	if r.calls == r.failAt {
		return host.View{}, errors.New("raw gateway failure password=synthetic-secret private-path")
	}
	return r.pagedTranscriptClient.Inspect(ctx, id, after, limit)
}

func TestExportHistoryFailureCleansPartialFileAndNeverLeaksRawError(t *testing.T) {
	f := conversationFixture("first", "second")
	// Three pages build the metadata index, then the first write page succeeds
	// before the second write page fails. No partial final/temp file may remain.
	r := &exportFailingReader{pagedTranscriptClient: f, failAt: 5}
	dir := filepath.Join(t.TempDir(), "exports")
	path, err := ExportConversation(t.Context(), r, "s", throughHistory(f), dir)
	var safe *ExportError
	if path != "" || !errors.As(err, &safe) || safe.Code != "history_unavailable" || strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "private-path") {
		t.Fatal("history failure not safely typed", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatal("partial export left after history failure", err)
	}
}

func TestExportCanonicalPrefixBoundaryAndQueuedRuntimeDoesNotRewriteTurns(t *testing.T) {
	f := conversationFixture("first response", "next response")
	// A queued selection is not the identity of an earlier/current Turn.
	queued := sessionstore.RuntimeSelection{Version: 1, Revision: 7, RequestID: "queued", Provider: "claude-code", Model: "synthetic-unapplied", Effort: "high"}
	f.view.History.Items = append(f.view.History.Items, host.HistoryItem{Sequence: 7, Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Kind: sessionstore.HostRuntimeSelection, Selection: &queued}})
	choices, err := ResponseChoices(t.Context(), f, "s", 3)
	if err != nil || len(choices) != 1 || choices[0].Preview != "first response" {
		t.Fatal("export snapshot included subsequent canonical messages", err)
	}
	dir := filepath.Join(t.TempDir(), "exports")
	path, err := ExportConversation(t.Context(), f, "s", 3, dir)
	if err != nil {
		t.Fatal(err)
	}
	if body := exportBody(t, path); !strings.Contains(body, "first response") || strings.Contains(body, "next response") {
		t.Fatal("snapshot prefix was not stable")
	}
	path, err = ExportConversation(t.Context(), f, "s", 7, dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(exportBody(t, path), "synthetic-unapplied") || strings.Contains(exportBody(t, path), "Provider:") {
		t.Fatal("pending selection guessed as prior turn metadata")
	}
}

func TestExportSafeFailuresPrivatePathsAndDefaultDirectory(t *testing.T) {
	f := conversationFixture("response")
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	path, err := ExportLastResponse(t.Context(), f, "../unsafe-session", throughHistory(f), "")
	if err != nil || filepath.Dir(path) != filepath.Join(root, "unreal-agent", "exports") || strings.Contains(filepath.Base(path), "unsafe") {
		t.Fatal("default export location/filename invalid", err)
	}
	if exportBody(t, path) != "response" {
		t.Fatal("default export body invalid")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("export file not 0600")
	}
	info, _ = os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0700 {
		t.Fatal("export directory not 0700")
	}
	blocked := filepath.Join(root, "password=private-path")
	if err := os.Mkdir(blocked, 0755); err != nil {
		t.Fatal(err)
	}
	_, err = ExportLastResponse(t.Context(), f, "s", throughHistory(f), blocked)
	var safe *ExportError
	if !errors.As(err, &safe) || strings.Contains(err.Error(), "private-path") {
		t.Fatal("raw filesystem error exposed", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ExportConversation(ctx, f, "s", throughHistory(f), "")
	if !errors.As(err, &safe) || safe.Code != "canceled" {
		t.Fatal("cancellation not safely typed", err)
	}
	empty := &pagedTranscriptClient{fakeClient: newFake(), pageSize: 128}
	if _, err = ExportLastResponse(t.Context(), empty, "s", 0, ""); !errors.As(err, &safe) || safe.Code != "no_responses" {
		t.Fatal("empty response export not typed", err)
	}
	if _, err = ExportConversation(t.Context(), empty, "s", 0, ""); !errors.As(err, &safe) || safe.Code != "no_conversation" {
		t.Fatal("empty conversation export not typed", err)
	}
}

func TestExportPickerNavigationResponsiveAndCommandDiscovery(t *testing.T) {
	var texts []string
	for i := range 30 {
		texts = append(texts, fmt.Sprintf("response-%02d 日本語 👩🏽‍💻", i))
	}
	f := conversationFixture(texts...)
	choices, err := ResponseChoices(t.Context(), f, "s", throughHistory(f))
	if err != nil {
		t.Fatal(err)
	}
	for _, theme := range []Theme{{}, {NoColor: true}, {Plain: true}, {Plain: true, ASCII: true}} {
		for _, size := range [][2]int{{44, 14}, {60, 20}, {80, 24}, {130, 32}} {
			p := responseExportPicker(choices)
			for _, direction := range []int{1, -1} {
				for range len(p.Options) {
					frame := RenderFrame(idleSnapshot(), UIState{Theme: theme, Picker: p}, size[0], size[1])
					exportFrameBounds(t, frame, size[0], size[1])
					if !strings.Contains(frameText(frame), fmt.Sprintf("Turn %d ", p.Responses[p.Selection].TurnNumber)) || frame.CursorY != size[1]-1 {
						t.Fatal("picker selection hidden or composer moved", size)
					}
					p.Offset = frame.PickerOffset
					p.move(direction)
				}
			}
		}
	}
	var names []string
	for _, candidate := range commandCatalog {
		if strings.HasPrefix(candidate.Name, "/copy") {
			t.Fatal("removed command still discoverable")
		}
		if strings.HasPrefix(candidate.Name, "/export") {
			names = append(names, candidate.Name)
		}
	}
	if strings.Join(names, ",") != "/export,/export last,/export all" {
		t.Fatal("export command discovery missing", names)
	}
	for _, short := range []bool{false, true} {
		help := strings.Join(helpSheet(Theme{}, short).Lines, "\n")
		if strings.Contains(help, "/copy") || !strings.Contains(help, "/export last") || !strings.Contains(help, "/export all") {
			t.Fatal("help outdated")
		}
	}
}
