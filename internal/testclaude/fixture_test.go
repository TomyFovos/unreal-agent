package testclaude

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCallsWaitForCompleteJSONLRecord(t *testing.T) {
	for _, name := range []string{"calls.jsonl", "auth-calls.jsonl", "catalog-calls.jsonl"} {
		t.Run(name, func(t *testing.T) {
			f := &Fixture{Directory: t.TempDir()}
			want := Call{Arguments: []string{}, PID: 123, Input: strings.Repeat("payload", 3000), Environment: []string{"SYNTHETIC=value"}}
			encoded, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			first := []byte(`{"PID":1}` + "\n")
			path := filepath.Join(f.Directory, name)
			write := func(data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Reproduce a read at the observed 8192-byte append boundary.
			write(append(append([]byte(nil), first...), encoded[:8192]...))
			if calls := f.calls(t, name); len(calls) != 1 || calls[0].PID != 1 {
				t.Fatal("unfinished append was exposed", calls)
			}
			write(append(append([]byte(nil), first...), encoded...))
			if calls := f.calls(t, name); len(calls) != 1 {
				t.Fatal("unterminated record was published", len(calls))
			}
			write(append(append(append([]byte(nil), first...), encoded...), '\n'))
			if calls := f.calls(t, name); len(calls) != 2 || !reflect.DeepEqual(calls[1], want) {
				t.Fatal("completed append lost its payload")
			}
		})
	}
}

func TestBatchContextsWaitForCompleteJSONLRecord(t *testing.T) {
	f := &Fixture{Directory: t.TempDir()}
	data := []byte(`{"hookSpecificOutput":{"additionalContext":"received"}}`)
	path := filepath.Join(f.Directory, "batches.jsonl")
	if err := os.WriteFile(path, data[:20], 0600); err != nil {
		t.Fatal(err)
	}
	if got := f.BatchContexts(t); len(got) != 0 {
		t.Fatal("unfinished batch exposed", got)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if got := f.BatchContexts(t); !reflect.DeepEqual(got, []string{"received"}) {
		t.Fatal("completed batch lost", got)
	}
}

type fatalFixtureTB struct{ testing.TB }
type fixtureFailure struct{ args []any }

func (f fatalFixtureTB) Fatal(args ...any) { panic(fixtureFailure{args}) }

func TestCompleteMalformedJSONLStillFails(t *testing.T) {
	f := &Fixture{Directory: t.TempDir()}
	if err := os.WriteFile(filepath.Join(f.Directory, "calls.jsonl"), []byte("{\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var failure any
	func() {
		defer func() { failure = recover() }()
		f.Calls(fatalFixtureTB{t})
	}()
	reported, ok := failure.(fixtureFailure)
	if !ok || !strings.Contains(fmt.Sprint(reported.args...), "unexpected EOF") {
		t.Fatalf("completed malformed record was not rejected: %v", failure)
	}
}
