package host

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestConfigurationProjectInstructionsFailClosed(t *testing.T) {
	const marker = "unique-configuration-fail-closed-secret-marker"
	snapshot, err := projectinstructions.FromContent([]byte(marker))
	if err != nil {
		t.Fatal(err)
	}
	metadata := snapshot.Metadata()
	badDigest := snapshot
	badDigest.Digest = "invalid-digest"
	mismatch := snapshot
	mismatch.Digest = "sha256:" + strings.Repeat("0", 64)
	badLength := snapshot
	badLength.ByteLength++
	future := snapshot
	future.Version++
	for _, tc := range []struct {
		name         string
		value        any
		wantMetadata bool
	}{
		{"valid", snapshot, true},
		{"invalid-digest", badDigest, false},
		{"digest-mismatch", mismatch, false},
		{"invalid-length", badLength, false},
		{"future-version", future, false},
		{"malformed-content", map[string]any{"Version": 1, "Content": map[string]string{"secret": marker}}, false},
		{"malformed-snapshot", marker, false},
		{"already-public-metadata", metadata, true},
		{"null", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"ProjectInstructions": tc.value, "Runtime": "retained"})
			if err != nil {
				t.Fatal(err)
			}
			store, err := localfile.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.CreateWithHostRecords(t.Context(), "configuration",
				sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: raw})
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.Items(t.Context(), "configuration", 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			canonical := page.Items[0]
			before, err := json.Marshal(canonical)
			if err != nil {
				t.Fatal(err)
			}
			check := func(item HistoryItem) {
				t.Helper()
				data, err := json.Marshal(item)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte(marker)) {
					t.Fatal("configuration exposed canonical instruction content")
				}
				var decoded HistoryItem
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				if decoded.Sequence != canonical.Sequence || decoded.RecordedAt != canonical.RecordedAt || decoded.Kind != canonical.Kind {
					t.Fatal("configuration projection changed history identity")
				}
				record := decoded.Data.(sessionstore.HostRecord)
				var fields map[string]jsontext.Value
				if err := json.Unmarshal(record.Configuration, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["Runtime"]) != "\"retained\"" {
					t.Fatal("unrelated configuration changed")
				}
				instructions, ok := fields["ProjectInstructions"]
				if !ok {
					t.Fatal("instruction field presence lost")
				}
				if tc.wantMetadata {
					var got projectinstructions.Metadata
					if err := json.Unmarshal(instructions, &got, json.RejectUnknownMembers(true)); err != nil {
						t.Fatal(err)
					}
					if got != metadata {
						t.Fatal("valid metadata changed during projection")
					}
				} else if string(instructions) != "null" {
					t.Fatalf("untrusted snapshot was not redacted: %s", instructions)
				}
			}
			check(ProjectHistoryItem(canonical))
			check(HistoryItem(canonical)) // Direct construction must not bypass redaction.
			check(ProjectHistoryItem(sessionstore.Item(ProjectHistoryItem(canonical))))
			observer := &Session{Generation: "configuration-events", running: true,
				submissions: map[inbox.ID]*submission{}, operations: map[operation.ID]operation.Operation{},
				subscribers: map[uint64]chan Event{}}
			observer.rememberItem(canonical, false)
			view, err := observer.Inspect(0, 1)
			if err != nil {
				t.Fatal(err)
			}
			if view.Revision != 0 || view.History.NextAfter != 1 || view.History.More || len(view.History.Items) != 1 {
				t.Fatal("inspection changed revision or cursor")
			}
			check(view.History.Items[0])
			sub, err := observer.Subscribe(0, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Cancel()
			check(sub.Initial.History.Items[0])
			eventItem := canonical
			eventItem.Sequence++
			observer.rememberItem(eventItem, true)
			event := <-sub.Events
			if event.Kind != "item" || event.Revision != 1 || event.Generation != observer.Generation || event.Item == nil {
				t.Fatal("configuration event semantics changed")
			}
			saved := canonical
			canonical = eventItem
			check(*event.Item)
			canonical = saved
			again, err := store.Items(t.Context(), "configuration", 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(again.Items[0])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || !bytes.Equal(raw, again.Items[0].Data.(sessionstore.HostRecord).Configuration) {
				t.Fatal("public projection changed canonical store bytes")
			}
		})
	}
}

func TestConfigurationProjectionUnreadableObjectFailsClosed(t *testing.T) {
	const marker = "unique-unreadable-configuration-secret-marker"
	raw := jsontext.Value(`[{"ProjectInstructions":{"Content":"` + marker + `"}}]`)
	record := sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: raw}
	item := sessionstore.Item{Sequence: 1, Kind: sessionstore.ItemHostRecord, Data: record}
	for _, public := range []HistoryItem{ProjectHistoryItem(item), HistoryItem(item)} {
		data, err := json.Marshal(public)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(marker)) {
			t.Fatal("unreadable configuration exposed raw content")
		}
		var decoded HistoryItem
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if string(decoded.Data.(sessionstore.HostRecord).Configuration) != "null" {
			t.Fatal("unreadable configuration did not fail closed")
		}
	}
	if !bytes.Equal(raw, record.Configuration) {
		t.Fatal("canonical unreadable configuration changed")
	}
}

func TestConfigurationProjectionWithoutInstructionsUnchanged(t *testing.T) {
	raw := jsontext.Value(`{"Runtime":"unchanged"}`)
	item := sessionstore.Item{Sequence: 1, Kind: sessionstore.ItemHostRecord,
		Data: sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: raw}}
	public := ProjectHistoryItem(item)
	if !bytes.Equal(public.Data.(sessionstore.HostRecord).Configuration, raw) {
		t.Fatal("unrelated configuration changed")
	}
	data, err := json.Marshal(HistoryItem(item))
	if err != nil {
		t.Fatal(err)
	}
	var decoded HistoryItem
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Data.(sessionstore.HostRecord).Configuration, raw) {
		t.Fatal("direct serialization changed unrelated configuration")
	}
}
