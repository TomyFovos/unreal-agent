package tui

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func TestChildMarksFollowCreationTimeAndID(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	rows := []viewer.Row{
		{ID: "parent", CreatedAt: now},
		{ID: "child-c", ParentID: "parent", CreatedAt: now.Add(time.Second)},
		{ID: "child-b", ParentID: "parent", CreatedAt: now},
		{ID: "child-a", ParentID: "child-b", CreatedAt: now},
	}
	original := slices.Clone(rows)
	want := map[session.ID]string{"child-a": "A", "child-b": "B", "child-c": "C"}
	if got := childMarks(rows); !maps.Equal(got, want) || !reflect.DeepEqual(rows, original) {
		t.Fatalf("marks = %v; caller rows mutated = %v", got, !reflect.DeepEqual(rows, original))
	}
	slices.Reverse(rows)
	rows[0].Finish = &viewer.Finish{Status: "success"}
	rows[1].ParentOperationStatus = "awaiting"
	if got := childMarks(rows); !maps.Equal(got, want) {
		t.Fatalf("recalculation changed marks: %v", got)
	}
}

func TestChildMarksExtendPastZAndZZ(t *testing.T) {
	var rows []viewer.Row
	for i := 0; i < 704; i++ {
		rows = append(rows, viewer.Row{ID: session.ID(fmt.Sprintf("child-%04d", i)), ParentID: "parent"})
	}
	slices.Reverse(rows)
	marks := childMarks(rows)
	for id, want := range map[session.ID]string{"child-0000": "A", "child-0001": "B", "child-0025": "Z", "child-0026": "AA", "child-0051": "AZ", "child-0052": "BA", "child-0701": "ZZ", "child-0702": "AAA", "child-0703": "AAB"} {
		if marks[id] != want {
			t.Fatalf("mark %s = %s, want %s", id, marks[id], want)
		}
	}
	if !maps.Equal(marks, childMarks(slices.Clone(rows))) {
		t.Fatal("reattach/recalculation changed labels")
	}
}
