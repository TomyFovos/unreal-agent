package record

import (
	"reflect"
	"testing"
)

func TestLocatorRoundTrip(t *testing.T) {
	for _, a := range []Address{
		{Kind: SessionItem, Session: "session-1", Sequence: 42},
		{Kind: UserMessage, Session: "session-1", Sequence: 42},
		{Kind: PeerMessage, Session: "session-1", Sequence: 42},
		{Kind: AssistantMessage, Session: "session-1", Sequence: 42, Part: 6},
		{Kind: ToolCall, Session: "session-1", Sequence: 42, Part: 2},
		{Kind: ChildReport, Session: "child-1", Sequence: 8, Handle: "h/?#", Section: "tests", Line: 2},
		{Kind: Artifact, Session: "child-1", Sequence: 8, Path: "/a path/例?#%", Hash: "hash"},
		{Kind: OperationOutput, Session: "child-1", Operation: "id/part", Stream: Stderr, Range: &ByteRange{Start: 2, End: 7}},
		{Kind: OperationCheckpoint, Session: "child-1", Operation: "id/part", Offset: 382},
	} {
		loc := a.Locator()
		b, err := loc.Address()
		if err != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("%q: got %+v (%v), want %+v", loc, b, err, a)
		}
	}
}

func TestInvalidLocators(t *testing.T) {
	for _, value := range []Locator{"rec:", "rec:a/0", "rec:a/01", "rec:a/2/call/-1", "rec:a/2/finish/", "rec:a/2/artifact/%xx/hash",
		"rec:a/output/op", "rec:a/operation/op/-1", "rec:a/2?bytes=4-3", "rec:a/2?line=0", "rec:a/2?line=1&bytes=0-1"} {
		if _, err := value.Address(); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}
