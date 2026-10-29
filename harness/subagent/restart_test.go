package subagent_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	sub "github.com/unreallabsai/unreal-agent/harness/subagent"
)

func TestHostRestartCancelsChildAndNotifiesParentOnce(t *testing.T) {
	dir := t.TempDir()
	h, parent := parentAt(t, dir, "wait", host.Create)
	active := awaitChild(t, parent, false)
	waitReady(t, parent)
	plan, err := sub.DecodePlan(active)
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	childLog := readLog(t, dir, plan.ChildID)
	for range 2 {
		h, parent = parentAt(t, dir, "wait", host.Resume)
		terminal := awaitChild(t, parent, true)
		if terminal.ID != active.ID || terminal.Status != operation.StatusCanceled {
			t.Fatalf("restored child=%+v", terminal)
		}
		deadline := time.After(5 * time.Second)
		for {
			v, err := parent.Inspect(0, 256)
			if err != nil {
				t.Fatal(err)
			}
			var ended []inbox.PeerMessage
			for _, item := range v.History.Items {
				if in, ok := item.Data.(inbox.Input); ok && in.Kind == inbox.InputPeer {
					peer, _ := in.DecodePeerMessage()
					if peer.Kind == "ended" {
						ended = append(ended, peer)
					}
				}
			}
			if len(ended) > 0 {
				if len(ended) != 1 || ended[0].Handle != string(active.ID) || ended[0].Sender != string(plan.ChildID) || !strings.Contains(ended[0].Text, "stopped because Unreal Agent restarted") || !strings.Contains(ended[0].Text, "subagent show "+string(active.ID)) {
					t.Fatalf("restart notifications=%+v", ended)
				}
				break
			}
			select {
			case <-deadline:
				t.Fatal("parent received no ended notice")
			case <-time.After(time.Millisecond):
			}
		}
		h.Close()
		if !bytes.Equal(readLog(t, dir, plan.ChildID), childLog) {
			t.Fatal("restart resumed the stopped child's execution")
		}
	}
}
