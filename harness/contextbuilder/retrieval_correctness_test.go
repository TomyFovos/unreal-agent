package contextbuilder

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
)

// The question deliberately does not contain the answer. Counting search hits
// or echoing the question is insufficient: the original source must be sent.
func TestHistoricalRetrievalOrbitLantern(t *testing.T) {
	for _, n := range []int{8, 80} {
		for _, reserve := range []int64{700, 12000} {
			t.Run(fmt.Sprintf("turns_%d_recent_%d", n, reserve), func(t *testing.T) {
				f := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 6000, RecentReserve: reserve, RetrievalLimit: 8, CheckpointThreshold: 4})
				original := "Context retrieval smoke.\nmarker ORBIT-LANTERN has value 4827-ALPHA."
				f.turn(t, "original", original, "Recorded the smoke marker.")
				for i := range n {
					question := fmt.Sprintf("Unrelated gardening question %d", i)
					if n > 8 && i%3 == 0 {
						question = fmt.Sprintf("Earlier question %d: What value was associated with ORBIT-LANTERN?", i)
					}
					f.turn(t, fmt.Sprint(i), question, strings.Repeat("Unrelated garden details. ", 12))
				}
				f.user(t, "current-question", "What value was associated with ORBIT-LANTERN?")
				before, _ := json.Marshal(f.history)
				r, err := f.b.Build()
				if err != nil {
					t.Fatal(err)
				}
				found, count := false, 0
				for _, s := range r.Package.Selected {
					if s.Reason != "retrieved" {
						continue
					}
					count++
					if s.Unit.Staged || s.Unit.Source.TurnID == "current-question" {
						t.Fatal("retrieved current question")
					}
					if contextengine.Text(s.Unit.Item) == original {
						found = true
					}
				}
				if !found || !strings.Contains(requestText(r), "4827-ALPHA") {
					t.Fatalf("original answer missing: recent=%d retrieved=%d omitted=%d estimate=%d", r.Report.Context.Recent, r.Report.Context.Retrieved, r.Report.Context.Omitted, r.Package.EstimatedTokens)
				}
				if count != r.Report.Context.Retrieved || r.Package.EstimatedTokens > 6000 {
					t.Fatal("incorrect selected count/budget")
				}
				if r.Report.Context.Retrieval.Selected != count || r.Report.Context.CheckpointBoundary < 4 {
					t.Fatal("selection diagnostics/checkpoint incorrect")
				}
				// Replay into a fresh derived generation, not a saved summary.
				rebuilt := newContextFixture(t, "claude-code", contextengine.Config{InputBudget: 6000, RecentReserve: reserve, RetrievalLimit: 8, CheckpointThreshold: 4})
				for _, item := range f.history {
					rebuilt.item(t, item.Kind, item.Data)
				}
				rb, err := rebuilt.b.Build()
				if err != nil || !reflect.DeepEqual(r.Package, rb.Package) {
					t.Fatal("canonical rebuild changed retrieval", err)
				}
				r2, err := f.b.Build()
				if err != nil || !reflect.DeepEqual(r.Package, r2.Package) {
					t.Fatal("non-deterministic package", err)
				}
				after, _ := json.Marshal(f.history)
				if string(before) != string(after) {
					t.Fatal("history changed")
				}
			})
		}
	}
}
