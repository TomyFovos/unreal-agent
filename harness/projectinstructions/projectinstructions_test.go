package projectinstructions

import (
	"encoding/json/v2"
	"testing"
)

func TestSnapshotRoundTripsAndValidatesIdentity(t *testing.T) {
	snapshot, err := FromContent([]byte("Use tabs.\n"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != snapshot {
		t.Fatalf("round trip = %+v, want %+v", decoded, snapshot)
	}
	if err = decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	if metadata := snapshot.Metadata(); metadata.Digest != snapshot.Digest || metadata.ByteLength != 10 {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestSnapshotValidateRejectsTampering(t *testing.T) {
	valid, err := FromContent([]byte("A"))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Snapshot){
		"content":      func(s *Snapshot) { s.Content = "B" },
		"digest":       func(s *Snapshot) { s.Digest = "sha256:00" },
		"length":       func(s *Snapshot) { s.ByteLength++ },
		"version":      func(s *Snapshot) { s.Version = 2 },
		"path":         func(s *Snapshot) { s.SourcePath = "../AGENTS.md" },
		"source":       func(s *Snapshot) { s.SourceKind = "claude_md" },
		"none-content": func(s *Snapshot) { *s = None(); s.Content = "A" },
		"binary":       func(s *Snapshot) { s.Content = "\x00" },
	} {
		t.Run(name, func(t *testing.T) {
			s := valid
			mutate(&s)
			if s.Validate() == nil {
				t.Fatalf("accepted %+v", s)
			}
		})
	}
}
