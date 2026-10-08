package contextengine

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func engineForTest(t *testing.T, config Config) *Engine {
	t.Helper()
	e, err := New(config, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.Configure(Runtime{Provider: "claude-code", Model: "synthetic", Effort: "medium", Revision: 7}, Constraints{ToolPolicy: "deny"}, true)
	return e
}
func buildForTest(t *testing.T, e *Engine) (Package, Diagnostics) {
	t.Helper()
	p, d, err := e.Build(BuildInput{Instructions: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "trusted instructions"}}})
	if err != nil {
		t.Fatal(err)
	}
	return p, d
}

func TestDerivedCheckpointAndIndexRebuildIgnoreBrokenCache(t *testing.T) {
	root := t.TempDir()
	history := []sessionstore.Item{}
	for i := 1; i <= 36; i++ {
		history = append(history, sessionstore.Item{Sequence: sessionstore.Sequence(i), RecordedAt: time.Unix(int64(i), 0).UTC(), Kind: sessionstore.ItemHostRecord, Data: sessionstore.HostRecord{Version: 1, Kind: "stop_complete"}})
	}
	rebuild := func() *Engine {
		e := engineForTest(t, Config{CheckpointThreshold: 4})
		for i, item := range history {
			text := "original retained public body"
			if i == 4 {
				text = "exact ancient ABC-1234"
			}
			key := string(rune(i + 65))
			e.Add(Unit{Kind: UserMessage, Class: Keep, Source: HistoryRef{SourceType: "input", TurnID: key}, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}}}, key)
			e.Observe(item, key)
		}
		e.Add(Unit{Kind: UserMessage, Class: Pin, Staged: true, Required: true, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Recall ABC-1234"}}}, "current")
		return e
	}
	e := rebuild()
	p, _ := buildForTest(t, e)
	for _, kind := range []string{"missing", "corrupt", "partial", "version", "stale"} {
		t.Run(kind, func(t *testing.T) {
			c := OpenCache(root, kind)
			t.Cleanup(func() { c.Close() })
			if err := c.Write(e.Manifest()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, DerivedDirectory, kind, DerivedFile)
			switch kind {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "corrupt", "partial":
				if err := os.WriteFile(path, []byte(`{"Version":1,"Checkpoint":`), 0600); err != nil {
					t.Fatal(err)
				}
			case "version":
				if err := os.WriteFile(path, []byte(`{"Version":99,"Checkpoint":{"Version":99}}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "stale":
				if err := os.WriteFile(path, []byte(`{"Version":1,"Checkpoint":{"Version":1,"ThroughSequence":999999,"State":{"Runtime":{"Model":"wrong-model"}}}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			loaded := OpenCache(root, kind)
			t.Cleanup(func() { loaded.Close() })
			e2 := rebuild()
			e2.SetCacheStatus(loaded.Status())
			p2, d := buildForTest(t, e2)
			if !reflect.DeepEqual(p, p2) {
				t.Fatal("derived cache changed canonical rebuild selection/checkpoint")
			}
			if err := loaded.Write(e2.Manifest()); err != nil {
				t.Fatal(err)
			}
			if d.CheckpointBoundary != 36 || d.CheckpointVersion != 1 {
				t.Fatal("checkpoint missing")
			}
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("cache not private")
			}
			if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0700 {
				t.Fatal("cache directory not private")
			}
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), "original retained public body") || strings.Contains(string(data), "exact ancient ABC-1234") || strings.Contains(string(data), "Recall ABC-1234") {
				t.Fatal("derived file contains original source bodies")
			}
		})
	}
}

func TestCacheRejectsUnsafePathsAndPinsWrites(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, DerivedDirectory)); err != nil {
		t.Fatal(err)
	}
	c := OpenCache(root, "test")
	if c.Status() != "unavailable" || c.Write(Manifest{Version: Version}) == nil {
		t.Fatal("accepted symlink derived directory")
	}
	root = t.TempDir()
	c = OpenCache(root, "test")
	defer c.Close()
	if err := c.Write(Manifest{Version: Version}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, DerivedDirectory, "test", DerivedFile)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	unsafe := OpenCache(root, "test")
	defer unsafe.Close()
	if unsafe.Status() != "unavailable" || unsafe.Write(Manifest{Version: Version}) == nil {
		t.Fatal("accepted public derived file")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	old := filepath.Dir(path) + "-pinned"
	if err := os.Rename(filepath.Dir(path), old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(Manifest{Version: Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, DerivedFile)); !os.IsNotExist(err) {
		t.Fatal("write escaped pinned directory")
	}
	if _, err := os.Stat(filepath.Join(old, DerivedFile)); err != nil {
		t.Fatal("lost pinned derived write")
	}
}

func TestBudgetKnownWindowFallbackSchemasAndErrors(t *testing.T) {
	e := engineForTest(t, Config{FallbackWindow: 20000, InputBudget: 10000, ResponseReserve: 3000, ProtocolReserve: 1000})
	p, _ := buildForTest(t, e)
	if p.Budget.Window != 20000 || p.Budget.WindowSource != "conservative-fallback" || p.Budget.Input != 10000 {
		t.Fatal(p.Budget)
	}
	input := BuildInput{Runtime: Runtime{Provider: "openai-codex", Model: "small", ContextWindow: 12000, Tools: true}, SchemaTokens: 2000, Instructions: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "instructions"}}}
	p, _, err := e.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	if p.Budget.Input != 6000 || p.Budget.WindowSource != "catalog" || p.Budget.SchemaReserve != 2000 {
		t.Fatal(p.Budget)
	}
	input.Runtime.ContextWindow = 6000
	if _, _, err = e.Build(input); err == nil {
		t.Fatal("over-reserved model context was accepted")
	}
	input.Runtime.ContextWindow = 6200
	if _, _, err = e.Build(input); !IsBudgetError(err) {
		t.Fatalf("required state was silently truncated: %v", err)
	}
	for _, c := range []Config{{Version: 2}, {Mode: "guess"}, {InputBudget: -1}, {FallbackWindow: 100}} {
		if _, err := New(c, nil, nil); err == nil {
			t.Fatal("invalid config accepted", c)
		}
	}
}

func TestLexicalExactCodeFilenameSymbolCJKAndDeterministicTies(t *testing.T) {
	l := NewLexical()
	for _, id := range []string{"z", "a", "b"} {
		l.Put(Unit{ID: id, Class: Keep, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "ABC-1234 src/file_name.go FunctionSymbol 日本語検索"}}})
	}
	for _, query := range []string{"ABC-1234", "src/file_name.go", "FunctionSymbol", "日本語"} {
		hits := l.Search(query, 2)
		if len(hits) != 2 || hits[0].ID != "a" || hits[1].ID != "b" {
			t.Fatal(query, hits)
		}
	}
	l.Remove("a")
	if hits := l.Search("ABC-1234", 10); len(hits) != 2 || hits[0].ID != "b" {
		t.Fatal(hits)
	}
}

func TestEngineRaceSafeMetadataAndImmutablePublicPayload(t *testing.T) {
	e := engineForTest(t, Config{})
	output := []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "original public tool receipt"}}
	e.Add(Unit{Kind: ToolResult, Class: Keep, Item: llm.Item{Type: llm.ItemToolResult, Data: llm.ToolResult{CallID: "call", Output: output}}}, "receipt")
	output[0].Value = "caller mutated original"
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Go(func() {
			for n := 0; n < 50; n++ {
				e.ObserveOperation(operation.Operation{ID: "op", Type: operation.TypeShell, Status: operation.StatusAwaiting})
				buildForTest(t, e)
				e.Manifest()
			}
		})
	}
	wg.Wait()
	p, _ := buildForTest(t, e)
	for _, s := range p.Selected {
		if v, ok := s.Unit.Item.Data.(llm.ToolResult); ok {
			if v.Output[0].Value != "original public tool receipt" {
				t.Fatal("input alias changed engine")
			}
			v.Output[0].Value = "mutated returned package"
		}
	}
	again, _ := buildForTest(t, e)
	for _, s := range again.Selected {
		if v, ok := s.Unit.Item.Data.(llm.ToolResult); ok && v.Output[0].Value != "original public tool receipt" {
			t.Fatal("package alias changed engine")
		}
	}
}

func TestStructuredStateAllowlistNeverCopiesOperationSecrets(t *testing.T) {
	e := engineForTest(t, Config{})
	state := []byte(`{"Plan":{"Type":"subagent","Data":{"Action":"start","Text":"private task marker","Configuration":{"Runtime":{"Provider":{"provider":"claude-code","model":{"id":"sonnet"},"Auth":{"token":"secret-auth-marker"}},"ReasoningEffort":"high","ClaudeCode":{"managedPolicyMode":"trust"}}}}},"Handle":{"ChildID":"child-synthetic","Finish":{"Result":{"Status":"completed","Summary":"private child body marker"}}},"TerminalResult":"private output marker","environment":{"ANTHROPIC_API_KEY":"secret-env-marker"}}`)
	e.ObserveOperation(operation.Operation{ID: "child-operation", Type: operation.TypeRemoteJob, Status: operation.StatusCompleted, State: state})
	p, _ := buildForTest(t, e)
	if len(p.State.Operations) != 1 || p.State.Operations[0].Provider != "claude-code" || p.State.Operations[0].Outcome != "completed" {
		t.Fatal(p.State)
	}
	data, _ := json.Marshal(p.State)
	for _, s := range []string{"private task marker", "secret-auth-marker", "private child body marker", "private output marker", "secret-env-marker", "managedPolicyMode"} {
		if strings.Contains(string(data), s) {
			t.Fatal("raw operation state entered context", s)
		}
	}
}

func TestActiveOperationMetadataDoesNotCrowdOutRequiredInput(t *testing.T) {
	e := engineForTest(t, Config{InputBudget: 2800})
	for i := 0; i < 100; i++ {
		e.ObserveOperation(operation.Operation{ID: operation.ID(fmt.Sprintf("active-operation-%03d", i)), Type: operation.TypeShell, ToolName: "Bash", Status: operation.StatusAwaiting})
	}
	input := "CURRENT REQUEST " + strings.Repeat("preserve original input ", 40)
	e.Add(Unit{Kind: UserMessage, Class: Pin, Required: true, Staged: true, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: input}}}, "input")
	p, d := buildForTest(t, e)
	if p.State.ActiveOperations != 100 || p.State.ReferencedOperationStates == 0 || len(p.State.Operations) >= 32 || p.EstimatedTokens > p.Budget.Input {
		t.Fatal("unbounded machine metadata displaced current input", d, p.State)
	}
	if len(p.Selected) != 1 || Text(p.Selected[0].Unit.Item) != input {
		t.Fatal("required input was omitted or rewritten")
	}
}
