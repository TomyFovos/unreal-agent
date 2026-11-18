package localfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestRestoreCheckpointMatchesCanonicalReplay(t *testing.T) {
	for seed := uint64(0); seed < 12; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			ctx := t.Context()
			store, id, _ := newRestoreFixture(t)
			checkpointWriteHead(t, store, id)
			rng := rand.New(rand.NewPCG(seed, seed))
			cut := rng.IntN(5)
			var previous session.TurnID
			var saved []byte
			for n := range 6 {
				if n == cut {
					checkpointWriteHead(t, store, id)
					saved = readCheckpointFixtureFile(t, store.checkpointPath(id))
				}
				turn := session.TurnID(fmt.Sprintf("turn-%d", n))
				mustCheckpoint(t, store.AppendTurn(ctx, id, session.Turn{ID: turn, PreviousTurnID: previous, Type: session.TurnRegular}))
				previous = turn
				response := llm.Response{Usage: llm.Usage{Raw: []byte(`{"attribution":{"items":{"test":9007199254740993}}}`)}}
				mustCheckpoint(t, store.AppendModelResponse(ctx, id, sessionstore.ModelResponse{TurnID: turn, Response: response}))
				op := operation.Operation{ID: operation.ID(turn), Type: "fixture", Version: 1, Status: operation.StatusReady, State: []byte(`{"order":0}`)}
				status := sessionstore.ToolCallStatus{TurnID: turn, CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{op}}
				mustCheckpoint(t, store.AppendToolCallStatus(ctx, id, status))
				for range rng.IntN(4) + 1 {
					op.Status = operation.StatusAwaiting
					op.State = fmt.Appendf(nil, `{"order":%d,"evidence":[9,8,7]}`, rng.IntN(100))
					mustCheckpoint(t, store.SaveOperation(ctx, id, op))
				}
				if rng.IntN(2) == 0 {
					op.Status = operation.StatusCompleted
					mustCheckpoint(t, store.SaveOperation(ctx, id, op))
					status.Operations = []operation.Operation{op}
					mustCheckpoint(t, store.AppendToolCallStatus(ctx, id, status))
				}
			}
			// A crash can leave committed canonical items and operations after
			// the old checkpoint. The body of a torn final append is ignored.
			file, err := os.OpenFile(store.sessionPath(id), os.O_WRONLY|os.O_APPEND, 0)
			mustCheckpoint(t, err)
			_, err = file.WriteString(`{"type":"operation","data":`)
			mustCheckpoint(t, err)
			mustCheckpoint(t, file.Close())
			if !bytes.Equal(saved, readCheckpointFixtureFile(t, store.checkpointPath(id))) {
				t.Fatal("fixture checkpoint advanced past the simulated crash")
			}
			assertCheckpointMatchesCanonical(t, store, id, true)
		})
	}
}

func TestRestoreCheckpointFallsBack(t *testing.T) {
	for _, damage := range []string{"missing", "corrupt", "partial", "old version", "changed canonical prefix", "invalid canonical tail"} {
		t.Run(damage, func(t *testing.T) {
			store, id, _ := newRestoreFixture(t)
			checkpointWriteHead(t, store, id)
			path := store.checkpointPath(id)
			data := readCheckpointFixtureFile(t, path)
			switch damage {
			case "missing":
				mustCheckpoint(t, os.Remove(path))
			case "corrupt":
				data[len(data)-1] ^= 1
				mustCheckpoint(t, os.WriteFile(path, data, 0o600))
			case "partial":
				mustCheckpoint(t, os.WriteFile(path, data[:len(data)/2], 0o600))
			case "old version":
				var c restoreCheckpoint
				mustCheckpoint(t, json.Unmarshal(data[:len(data)-sha256.Size], &c))
				c.Version--
				old, err := encodeRestoreCheckpoint(c)
				mustCheckpoint(t, err)
				mustCheckpoint(t, os.WriteFile(path, old, 0o600))
			case "changed canonical prefix":
				canonical := readCheckpointFixtureFile(t, store.sessionPath(id))
				updated := bytes.Replace(canonical, []byte("synthetic"), []byte("different"), 1)
				mustCheckpoint(t, os.WriteFile(store.sessionPath(id), updated, 0o600))
			case "invalid canonical tail":
				path := store.sessionPath(id)
				canonical := readCheckpointFixtureFile(t, path)
				mustCheckpoint(t, os.WriteFile(path, append(canonical, []byte("{}\n")...), 0o600))
				if _, err := store.OpenCheckpointedRestoreView(t.Context(), id); err == nil {
					t.Fatal("checkpoint hid a committed invalid record")
				}
				return
			}
			assertCheckpointMatchesCanonical(t, store, id, false)
		})
	}
}

func TestRestoreCheckpointForkReusesOnlyChildOperations(t *testing.T) {
	ctx := t.Context()
	store, parent, _ := newRestoreFixture(t)
	const id = session.ID("fork")
	mustCheckpoint(t, store.AppendTurn(ctx, parent, session.Turn{ID: "parent-turn", Type: session.TurnRegular}))
	op := operation.Operation{ID: "old", Type: "fixture", Version: 1, Status: operation.StatusReady}
	mustCheckpoint(t, store.AppendToolCallStatus(ctx, parent, sessionstore.ToolCallStatus{
		TurnID: "parent-turn", CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{op.ID}}, Operations: []operation.Operation{op},
	}))
	_, err := store.Fork(ctx, id, parent, "parent-turn")
	mustCheckpoint(t, err)
	lease, err := store.AcquireWriter(id)
	mustCheckpoint(t, err)
	defer lease.Close()
	checkpointWriteHead(t, store, id)
	mustCheckpoint(t, store.AppendTurn(ctx, id, session.Turn{ID: "fork-turn", PreviousTurnID: "parent-turn", Type: session.TurnRegular}))
	mustCheckpoint(t, store.AppendInput(ctx, id, inbox.Input{ID: "fork-in", Kind: inbox.InputExternal, Payload: []byte(`"continue"`)}))
	assertCheckpointMatchesCanonical(t, store, id, true)
}

func TestRestoreCheckpointPublicationDoesNotGateCanonicalAppend(t *testing.T) {
	store, id, _ := newRestoreFixture(t)
	ctx := t.Context()
	v, err := store.OpenCheckpointedRestoreView(ctx, id)
	mustCheckpoint(t, err)
	_, _, err = v.Take(ctx, id)
	mustCheckpoint(t, err)
	path := store.checkpointPath(id)
	// Prevent the cache rename on the periodic publication boundary.
	mustCheckpoint(t, os.Mkdir(path, 0o700))
	index := store.writeStateCache[id].restoreIndex
	index.appends = restoreCheckpointInterval - 1
	mustCheckpoint(t, store.AppendInput(ctx, id, inbox.Input{ID: "committed", Kind: inbox.InputExternal, Payload: []byte(`"next"`)}))
	if index.savedSize != 0 || index.appends != 0 {
		t.Fatal("fixture did not exercise a failed periodic publication")
	}
	assertCheckpointMatchesCanonical(t, store, id, false)
	mustCheckpoint(t, os.Remove(path))
	index.appends = restoreCheckpointInterval - 1
	mustCheckpoint(t, store.AppendInput(ctx, id, inbox.Input{ID: "after-error", Kind: inbox.InputExternal, Payload: []byte(`"continue"`)}))
	assertCheckpointMatchesCanonical(t, store, id, true)
}

func checkpointWriteHead(t *testing.T, store *Store, id session.ID) {
	t.Helper()
	view, err := store.OpenCheckpointedRestoreView(t.Context(), id)
	mustCheckpoint(t, err)
	_, _, err = view.Take(t.Context(), id)
	mustCheckpoint(t, err)
	mustCheckpoint(t, store.SaveRestoreCheckpoint(id))
}

func assertCheckpointMatchesCanonical(t *testing.T, store *Store, id session.ID, hit bool) {
	t.Helper()
	before := readCheckpointFixtureFile(t, store.sessionPath(id))
	canonical, size, err := decodeLog(before)
	mustCheckpoint(t, err)
	// Fresh Store, like the next process after acquiring the writer.
	reopened, err := New(store.directory)
	mustCheckpoint(t, err)
	v, err := reopened.OpenCheckpointedRestoreView(t.Context(), id)
	mustCheckpoint(t, err)
	defer v.Close()
	if got := v.index.savedSize != 0; got != hit {
		t.Fatalf("checkpoint hit = %v, want %v", got, hit)
	}
	if v.committedSize != size || !reflect.DeepEqual(*v.state, canonical) {
		t.Fatal("checkpoint changed the canonical write head or timeline")
	}
	if !bytes.Equal(before, readCheckpointFixtureFile(t, store.sessionPath(id))) {
		t.Fatal("opening the checkpoint rewrote canonical history")
	}
}

func readCheckpointFixtureFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	mustCheckpoint(t, err)
	return data
}

func mustCheckpoint(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
