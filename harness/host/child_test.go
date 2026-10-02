package host

import (
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"testing"
)

func TestPeerCanonicalProvenanceDedupAndResume(t *testing.T) {
	h := newTestHost(t, t.TempDir())
	s, err := h.Create(t.Context(), Options{ID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "parent", Handle: "operation", Kind: "ready", Text: "ready"})
	in := inbox.Input{ID: "ready-operation", Kind: inbox.InputPeer, Payload: data}
	if _, err = s.Submit(t.Context(), s.Generation, in); err == nil {
		t.Fatal("unbound peer accepted")
	}
	if _, err = s.SubmitPeer(t.Context(), s.Generation, "spoof", "operation", in); err == nil {
		t.Fatal("spoof accepted")
	}
	first, err := s.SubmitPeer(timeout(t), s.Generation, "child", "operation", in)
	if err != nil {
		t.Fatal(err)
	}
	stop(t, s)
	s, err = h.Resume(t.Context(), Options{ID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.SubmitPeer(timeout(t), s.Generation, "child", "operation", in)
	if err != nil || first != again {
		t.Fatal(first, again, err)
	}
	data, _ = json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "parent", Handle: "operation", Kind: "ready", Text: "mutated"})
	in.Payload = data
	if _, err = s.SubmitPeer(timeout(t), s.Generation, "child", "operation", in); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	stop(t, s)
}
