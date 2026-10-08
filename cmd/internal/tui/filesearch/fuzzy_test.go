package filesearch

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestFuzzyFilenamePriorityStableTiesUnicodeAndPaths(t *testing.T) {
	index := Index{files: []string{"renderer/render.go", "vendor/editor.go", "cmd/editor.go", "cmd/Editor.go", "editor/nothing.go", "docs/猫 👩🏽‍💻é.md", "cmd/no.go"}}
	for _, test := range []struct {
		query string
		first string
	}{
		{"editor.go", "cmd/editor.go"}, {"edgo", "cmd/editor.go"},
		{"cmd/ed", "cmd/editor.go"}, {"./cmd/ed", "cmd/editor.go"},
		{"猫👩🏽‍💻", "docs/猫 👩🏽‍💻é.md"}, {"é", "docs/猫 👩🏽‍💻é.md"},
	} {
		m, err := index.Search(context.Background(), test.query, 32)
		if err != nil || len(m) == 0 || m[0].Path != test.first {
			t.Fatalf("%q %v %v", test.query, m, err)
		}
		// Source order cannot affect ranking/ties.
		reversed := Index{files: append([]string(nil), index.files...)}
		for i, j := 0, len(reversed.files)-1; i < j; i, j = i+1, j-1 {
			reversed.files[i], reversed.files[j] = reversed.files[j], reversed.files[i]
		}
		m2, _ := reversed.Search(context.Background(), test.query, 32)
		if !reflect.DeepEqual(m, m2) {
			t.Fatal("nondeterministic ranking")
		}
	}
}

func TestSearchResultAndQueryBounds(t *testing.T) {
	var index Index
	for i := 0; i < 10000; i++ {
		index.files = append(index.files, fmt.Sprintf("folder/file-%05d.go", i))
	}
	for _, limit := range []int{0, 1, 8, 32, 1000000} {
		m, err := index.Search(context.Background(), "file", limit)
		if err != nil || len(m) != min(limit, ResultLimit) {
			t.Fatal(limit, len(m), err)
		}
	}
	m, err := index.Search(context.Background(), strings.Repeat("x", QueryLimit+1), 32)
	if err != nil || len(m) != 0 {
		t.Fatal(m, err)
	}
	m, err = index.Search(context.Background(), "not-matching", 32)
	if err != nil || len(m) != 0 {
		t.Fatal(m, err)
	}
}
