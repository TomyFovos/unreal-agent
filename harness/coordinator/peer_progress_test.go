package coordinator

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func progressPeerInput(t *testing.T, id inbox.ID, kind, text string) inbox.Input {
	t.Helper()
	payload, err := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "child", Target: "session-1", Handle: "child-op", Kind: kind, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return inbox.Input{ID: id, Kind: inbox.InputPeer, Payload: payload}
}

func TestChildProgressWaitsForNextParentTurn(t *testing.T) {
	for _, delivery := range []string{"idle", "during_model", "during_claude_bridge"} {
		for _, wake := range []string{"message", "ended", "user"} {
			t.Run(delivery+"/"+wake, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					run := newStopTestRun(t, 0)
					run.start(t)
					run.input(t, externalEvent(t, 0, "task", "delegate work"))
					if delivery == "during_claude_bridge" {
						options := run.current.dependencies.LLM.(*fakeAdapter).requestOptionsSnapshot()[0]
						if err := options.BeginTools(run.calls[0].ctx, false); err != nil {
							t.Fatal(err)
						}
					}
					if delivery == "idle" {
						run.respond(t, 0, textResponse("delegated"))
					}
					for _, id := range []inbox.ID{"progress-one", "progress-two"} {
						run.input(t, progressPeerInput(t, id, "progress", string(id)))
						if len(run.calls) != 1 || run.current.wakingInputs() != 0 && delivery == "idle" {
							t.Fatal("progress woke the parent", len(run.calls), run.current.wakingInputs())
						}
					}
					if delivery != "idle" {
						run.respond(t, 0, textResponse("delegated"))
					}
					if len(run.calls) != 1 || run.current.wakingInputs() != 0 {
						t.Fatal("progress started a follow-up model call", len(run.calls), run.current.wakingInputs())
					}
					if len(run.store.appendedInputs) != 3 {
						t.Fatal("progress was not recorded", run.store.appendedInputs)
					}
					for _, input := range run.store.appendedInputs[1:] {
						if peer, err := input.DecodePeerMessage(); err != nil || peer.Kind != "progress" {
							t.Fatal("recorded progress lost its kind", peer, err)
						}
					}
					if wake == "user" {
						run.input(t, externalEvent(t, 1, "wake", "review the work"))
					} else {
						run.input(t, progressPeerInput(t, "wake", wake, "needs review"))
					}
					if len(run.calls) != 2 {
						t.Fatalf("%s started %d calls, want 2", wake, len(run.calls))
					}
					request := requestDump(t, run.calls[1].request)
					for _, text := range []string{"progress-one", "progress-two"} {
						if strings.Count(request, text) != 1 {
							t.Fatalf("next turn must contain progress once: %s", request)
						}
					}
					if run.current.backgroundTurn != (wake != "user") {
						t.Fatal("incorrect turn origin")
					}
					run.respond(t, 1, textResponse("reviewed"))
					if len(run.calls) != 2 || run.current.pendingInputs() != 0 {
						t.Fatal("progress was not consumed with the waking input")
					}
					run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
					run.assertStopped(t)
				})
			})
		}
	}
}

func TestRestoredChildProgressWaitsForUser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.store.items = []sessionstore.Item{
			storedItem(1, sessionstore.ItemInput, externalEvent(t, 0, "task", "delegate")),
			storedItem(2, sessionstore.ItemTurn, session.Turn{ID: "original", Type: session.TurnRegular}),
			storedItem(3, sessionstore.ItemModelResponse, sessionstore.ModelResponse{TurnID: "original", Response: textResponse("delegated")}),
			storedItem(4, sessionstore.ItemInput, progressPeerInput(t, "progress", "progress", "saved progress")),
		}
		run.start(t)
		if len(run.calls) != 0 || run.current.pendingInputs() != 1 || run.current.wakingInputs() != 0 {
			t.Fatal("restored progress woke the parent or was lost")
		}
		run.input(t, externalEvent(t, 1, "user", "review"))
		if len(run.calls) != 1 || !strings.Contains(requestDump(t, run.calls[0].request), "saved progress") {
			t.Fatal("next user turn lost restored progress")
		}
		run.respond(t, 0, textResponse("reviewed"))
		run.input(t, stopInput(t, "stop", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}
