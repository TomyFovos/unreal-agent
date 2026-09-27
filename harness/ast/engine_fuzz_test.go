//go:build cgo

package ast

import (
	"context"
	"testing"
)

func FuzzStructuralQuery(f *testing.F) {
	f.Add("old(1);", `(call_expression) @match`, "updated(1)")
	f.Add("function f( {", `(identifier) @match`, "${match}")
	f.Add("old(old(1));", `(call_expression) @match`, "")
	f.Fuzz(func(t *testing.T, source, query, template string) {
		if len(source) > 4096 || len(query) > 1024 || len(template) > 1024 {
			t.Skip()
		}
		result, err := analyze(context.Background(), "javascript", query, []byte(source), &template)
		if err == nil {
			if len(result.matches) > MaxMatches || len(result.content) > MaxSourceBytes {
				t.Fatal("unbounded result")
			}
			for i, match := range result.matches {
				if match.start < 0 || match.end > len(source) || match.end <= match.start || (i > 0 && match.start < result.matches[i-1].end) {
					t.Fatal("invalid ranges")
				}
			}
		}
	})
}
