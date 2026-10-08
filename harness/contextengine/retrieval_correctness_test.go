package contextengine

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestRetrievalEligibilityBeforeRankingAndLimit(t *testing.T) {
	e := engineForTest(t, Config{InputBudget: 6000, RecentReserve: 600, RetrievalLimit: 1})
	add := func(text, turn string, staged, required bool) string {
		return e.Add(Unit{Kind: UserMessage, Class: Keep, Staged: staged, Required: required, Source: HistoryRef{TurnID: turn}, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}}}, turn)
	}
	old := add("marker ORBIT-LANTERN has value 4827-ALPHA.", "original", false, false)
	for i := range 80 {
		add(fmt.Sprintf("question %d: What value was associated with ORBIT-LANTERN?", i), fmt.Sprint(i), false, false)
	}
	for i := range 20 {
		add(fmt.Sprintf("staged echo %d ORBIT-LANTERN", i), "staged", true, false)
	}
	current := add("What value was associated with ORBIT-LANTERN?", "current", true, true)
	p, d := buildForTest(t, e)
	if d.Retrieved != 1 || d.Retrieval.Selected != 1 || d.Retrieval.SkippedIneligible < 21 {
		t.Fatal("ineligible sources consume ranking slots", d.Retrieval)
	}
	found := false
	for _, s := range p.Selected {
		if s.Reason == "retrieved" {
			if s.Unit.ID == current || s.Unit.Staged {
				t.Fatal("retrieved current/staged input")
			}
			found = s.Unit.ID == old
		}
	}
	if !found {
		t.Fatal("original evidence lost")
	}

	// A committed current turn and its already observed output are not old
	// evidence. They must remain eligible for recent rendering, not retrieval.
	e.Commit("current")
	e.Add(Unit{Kind: ModelMessage, Class: Keep, Source: HistoryRef{TurnID: "current"}, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "ORBIT-LANTERN current output"}}}, "current-output")
	p, _ = buildForTest(t, e)
	for _, s := range p.Selected {
		if s.Reason == "retrieved" && s.Unit.Source.TurnID == "current" {
			t.Fatal("current turn contaminated historical retrieval")
		}
	}
}

func TestRetrievalOverlapBudgetAndSelectedDiagnostics(t *testing.T) {
	e := engineForTest(t, Config{InputBudget: 4000, RecentReserve: 300, RetrievalLimit: 3})
	original := strings.Repeat("padding ", 500) + "ORBIT-LANTERN value 4827-ALPHA" + strings.Repeat(" more", 400)
	whole := e.Add(Unit{Kind: ModelMessage, Class: Keep, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: original}}}, "old")
	for _, span := range [][2]int{{3800, 4250}, {3950, 4400}, {4300, 4600}} {
		e.Add(Unit{Kind: ModelMessage, Class: Keep, RetrievalOnly: true, ParentID: whole, Source: HistoryRef{StartByte: span[0], EndByte: span[1]}, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: original[span[0]:span[1]]}}}, "old")
	}
	e.Add(Unit{Kind: UserMessage, Class: Keep, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "historical ORBIT-LANTERN companion evidence"}}}, "other")
	e.Add(Unit{Kind: UserMessage, Class: Pin, Staged: true, Required: true, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Recall ORBIT-LANTERN"}}}, "current")
	p, d := buildForTest(t, e)
	count := 0
	found := false
	var kept []Selected
	for _, s := range p.Selected {
		if s.Reason == "retrieved" {
			count++
			if overlapsSelected(s.Unit, kept) {
				t.Fatal("overlap consumed retrieval budget")
			}
			kept = append(kept, s)
			found = found || strings.Contains(Text(s.Unit.Item), "4827-ALPHA")
		}
	}
	if !found || count != d.Retrieved || count != d.Retrieval.Selected || d.Retrieval.SkippedBudget == 0 || d.Retrieval.SkippedDuplicate == 0 || p.EstimatedTokens > p.Budget.Input {
		t.Fatal("overlap/budget/evidence diagnostics incorrect", d.Retrieval)
	}
	p2, _ := buildForTest(t, e)
	if !reflect.DeepEqual(p, p2) {
		t.Fatal("not deterministic")
	}
}

func TestLexicalExactLiteralAndSymbolPreferOriginalSource(t *testing.T) {
	for _, identifier := range []string{"ORBIT-LANTERN", "ABC-1234", "src/historical.go", "PreserveHistoricalEvidence", "`uniqueLiteral`"} {
		t.Run(identifier, func(t *testing.T) {
			l := NewLexical()
			l.Put(Unit{ID: "a", Class: Keep, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "Original evidence for " + identifier + " is the retained orchid invariant."}}})
			l.Put(Unit{ID: "b", Class: Keep, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "What was the exact original evidence for " + identifier + "?"}}})
			l.Put(Unit{ID: "c", Class: Keep, Item: llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "What was the exact original evidence for " + identifier + "?"}}})
			query := "What was the exact original evidence for " + identifier + "?"
			hits := l.Search(query, 1, func(id string) bool { return id != "c" })
			if len(hits) != 1 || hits[0].ID != "a" {
				t.Fatal("later question outranked original identifier evidence", hits)
			}
		})
	}
}
