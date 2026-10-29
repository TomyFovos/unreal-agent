package host

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func savedCheckpointFixture(raw jsontext.Value) operation.Operation {
	return operation.Operation{ID: "synthetic-checkpoint", Type: operation.TypeValue, Version: operation.VersionValue, Status: operation.StatusReady, State: raw, MaxOutputLength: operation.DefaultMaxOutputLength}
}

func savedCheckpointAdmit(tb testing.TB, store *localfile.Store, id session.ID, current operation.Operation) {
	tb.Helper()
	turn := session.Turn{ID: "synthetic-checkpoint-turn", Type: session.TurnRegular}
	if err := store.AppendTurn(tb.Context(), id, turn); err != nil {
		tb.Fatal(err)
	}
	response := llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "synthetic-checkpoint-call", Name: "synthetic", Arguments: `{}`}}}}
	if err := store.AppendModelResponse(tb.Context(), id, sessionstore.ModelResponse{TurnID: turn.ID, Response: response}); err != nil {
		tb.Fatal(err)
	}
	status := sessionstore.ToolCallStatus{TurnID: turn.ID, CallID: "synthetic-checkpoint-call", Status: tool.CallStatus{WaitingFor: []operation.ID{current.ID}}, Operations: []operation.Operation{current}}
	if err := store.AppendToolCallStatus(tb.Context(), id, status); err != nil {
		tb.Fatal(err)
	}
}

func TestSavedOperationCheckpointMatchesJSONCloneOracle(t *testing.T) {
	states := []jsontext.Value{
		jsontext.Value(`{"unknown":true,"n":1.2300e+2,"s":"\u65e5日本語<>&"}`),
		jsontext.Value("{\"literal\":\"日本語\u2028\u2029<>& white space\",\"n\":1E+09,\"large\":9007199254740993,\"zero\":-0}"),
		jsontext.Value(" { \"a\" : [1, 2], \"z\" : null } \n"),
		jsontext.Value(`null`), jsontext.Value(`"\u0041"`), jsontext.Value(`[]`),
	}
	metadata := []operation.Operation{
		{},
		{Execution: &operation.ExecutionMetadata{}, Idempotence: &operation.IdempotencyMetadata{}},
		{Execution: &operation.ExecutionMetadata{Dependencies: []operation.ID{}}},
		{Execution: &operation.ExecutionMetadata{Dependencies: []operation.ID{"prior"}}, Idempotence: &operation.IdempotencyMetadata{Idempotency: jsontext.Value(` {"k":3.0} `)}, Permission: &permission.Decision{Mode: "default", State: "granted"}, Denial: &permission.Error{Code: "denied", Capability: "synthetic", Reason: "test"}},
	}
	for i, raw := range states {
		for j, meta := range metadata {
			for _, canonicalInput := range []bool{false, true} {
				t.Run(fmt.Sprintf("state%d/meta%d/canonical%v", i, j, canonicalInput), func(t *testing.T) {
					previous := clone(savedCheckpointFixture(raw))
					value := savedCheckpointFixture(raw.Clone())
					if canonicalInput {
						value.State = previous.State.Clone()
					}
					value.Execution, value.Idempotence, value.Permission, value.Denial = meta.Execution, meta.Idempotence, meta.Permission, meta.Denial
					want := clone(value)
					got := cloneSavedOperationCheckpoint(value, previous)
					if !reflect.DeepEqual(got, want) {
						t.Fatal("private clone changed old JSON normalization", got, want)
					}
				})
			}
		}
	}
}

func TestSavedOperationCheckpointRestoredOpaqueStateKeepsNormalization(t *testing.T) {
	for _, raw := range []jsontext.Value{
		jsontext.Value(" { \"restored\" : true, \"n\" : 1.2300e+2 } "),
		jsontext.Value(`{"escaped":"\u0041\/\n","n":-0}`),
		jsontext.Value(`{"literal":"日本語 white space","n":1.2300e+2,"z":-0}`),
		jsontext.Value("{\"literal\":\"日本語\u2028\u2029<>& white space\",\"n\":1E+09,\"large\":9007199254740993,\"zero\":-0}"),
	} {
		// Startup RestoreView owns a deep copy but intentionally preserves opaque
		// lexemes. It does not establish prior host.clone normalization.
		previous := savedCheckpointFixture(raw.Clone())
		value := previous
		value.State = raw.Clone()
		got := cloneSavedOperationCheckpoint(value, previous)
		want := clone(value)
		if !reflect.DeepEqual(got, want) {
			t.Fatal("restored opaque State changed old normalization", got, want)
		}
		if !unchangedJSONFormatting(raw) && &got.State[0] == &previous.State[0] {
			t.Fatal("unnormalized/escaped State used private reuse")
		}
	}
}

func TestSavedOperationFormattingEligibilityOracle(t *testing.T) {
	corpus := []jsontext.Value{
		jsontext.Value(`{"code":"line1\n\t\"quoted\"\\path\r\b\f","n":1E+09,"z":-0}`),
		jsontext.Value(`{"nonminimal":"\/\u0041\u000a"}`),
		jsontext.Value(`{"literal":"\\u0041"}`),
		jsontext.Value(`null`), jsontext.Value(`false`), jsontext.Value(`9007199254740993`),
	}
	for r := rune(0); r < 128; r++ {
		encoded, err := json.Marshal(string(r))
		if err != nil {
			t.Fatal(err)
		}
		corpus = append(corpus, jsontext.Value(append(append([]byte(`{"s":`), encoded...), '}')))
	}
	for _, raw := range corpus {
		if !raw.IsValid() {
			t.Fatal("invalid synthetic formatting corpus")
		}
		want := clone(savedCheckpointFixture(raw)).State
		if unchangedJSONFormatting(raw) && !bytes.Equal(raw, want) {
			t.Fatal("eligible raw State would change old JSON normalization", string(raw), string(want))
		}
	}
	for _, raw := range []jsontext.Value{jsontext.Value(`"line1\n\t\"quoted\"\\path\r\b\f"`), jsontext.Value(`{"literal":"\\u0041"}`)} {
		if !unchangedJSONFormatting(raw) {
			t.Fatal("minimal common code escapes unnecessarily fell back")
		}
	}
	for _, raw := range []jsontext.Value{jsontext.Value(`"\/"`), jsontext.Value(`"\u0041"`), jsontext.Value(" true "), jsontext.Value(`"unfinished`)} {
		if unchangedJSONFormatting(raw) {
			t.Fatal("nonminimal/unfinished raw State was eligible")
		}
	}
}

func TestSavedOperationCheckpointDetachesIncomingMetadataAndBytes(t *testing.T) {
	previous := clone(savedCheckpointFixture(jsontext.Value(`{"owned":true}`)))
	value := previous
	value.State = previous.State.Clone()
	value.Execution = &operation.ExecutionMetadata{Dependencies: []operation.ID{"prior"}}
	value.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value(`{"key":3}`)}
	value.Permission = &permission.Decision{Mode: "default", State: "granted"}
	value.Denial = &permission.Error{Code: "denied", Capability: "synthetic", Reason: "test"}
	want := clone(value)
	got := cloneSavedOperationCheckpoint(value, previous)
	if &got.State[0] != &previous.State[0] {
		t.Fatal("metadata-only checkpoint did not reuse its private owner State")
	}
	value.State[0] = '['
	value.Execution.Dependencies[0] = "changed"
	value.Idempotence.Idempotency[0] = '['
	value.Permission.State = "changed"
	value.Denial.Reason = "changed"
	if !reflect.DeepEqual(got, want) || !bytes.Equal(previous.State, want.State) {
		t.Fatal("incoming mutable values aliased private owner checkpoint")
	}
}

func TestSavedOperationCheckpointFallbackAndErrorsMatchOracle(t *testing.T) {
	previous := clone(savedCheckpointFixture(jsontext.Value(`{"owner":true}`)))
	for _, raw := range []jsontext.Value{nil, {}, jsontext.Value(` {"changed":true} `), jsontext.Value(`{"x":1,"x":2}`), jsontext.Value("\"\xff\""), jsontext.Value(strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000))} {
		value := savedCheckpointFixture(raw)
		var want, got operation.Operation
		capture := func(f func()) (result any) { defer func() { result = recover() }(); f(); return nil }
		wantError := capture(func() { want = clone(value) })
		gotError := capture(func() { got = cloneSavedOperationCheckpoint(value, previous) })
		if fmt.Sprintf("%T:%v", gotError, gotError) != fmt.Sprintf("%T:%v", wantError, wantError) || !reflect.DeepEqual(got, want) {
			t.Fatal("fallback changed old value or exact error", gotError, wantError)
		}
	}
	value := previous
	value.State = previous.State.Clone()
	value.ID = "different-id"
	got := cloneSavedOperationCheckpoint(value, previous)
	if &got.State[0] == &previous.State[0] || &got.State[0] == &value.State[0] || !reflect.DeepEqual(got, clone(value)) {
		t.Fatal("different operation reused private State")
	}
}

func TestSavedOperationCheckpointKeepsFullWireAndPublicDetachment(t *testing.T) {
	directory := t.TempDir()
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("synthetic-clone-session")
	if _, err = store.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 4)
	s := &Session{ID: id, Generation: "synthetic-generation", store: store, operations: map[operation.ID]operation.Operation{}, subscribers: map[uint64]chan Event{1: events}}
	w := &serializedStore{s}
	initial := savedCheckpointFixture(jsontext.Value(` { "owner" : true } `))
	savedCheckpointAdmit(t, store, id, initial)
	if err = w.SaveOperation(t.Context(), id, initial); err != nil {
		t.Fatal(err)
	}
	previous := s.operations[initial.ID]
	checkpoint := previous
	checkpoint.State = previous.State.Clone()
	checkpoint.Execution = &operation.ExecutionMetadata{}
	if err = w.SaveOperation(t.Context(), id, checkpoint); err != nil {
		t.Fatal(err)
	}
	owned := s.operations[checkpoint.ID]
	if &owned.State[0] != &previous.State[0] || !reflect.DeepEqual(owned, clone(checkpoint)) {
		t.Fatal("saved checkpoint lost private reuse/normalization")
	}
	encoded, err := os.ReadFile(filepath.Join(directory, string(id)+".session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(encoded, []byte{'\n'}), []byte{'\n'})
	wire, err := json.Marshal(struct {
		Type string                                  `json:"type"`
		Data struct{ Operation operation.Operation } `json:"data"`
	}{Type: "operation", Data: struct{ Operation operation.Operation }{checkpoint}})
	if err != nil || !bytes.Equal(lines[len(lines)-1], wire) {
		t.Fatal("full canonical wire changed", err)
	}
	checkpoint.State[0] = '['
	for i := 0; i < 2; i++ {
		event := <-events
		event.Operation.State[0] = '['
		if event.Operation.Execution != nil {
		}
	}
	view, err := s.Inspect(0, 1)
	if err != nil || len(view.Operations) != 1 {
		t.Fatal("public view unavailable", err)
	}
	view.Operations[0].State[0] = '['
	if !bytes.Equal(s.operations[owned.ID].State, owned.State) || !s.operations[owned.ID].State.IsValid() {
		t.Fatal("public event/view or incoming worker changed private checkpoint")
	}
	resumed, err := store.Resume(t.Context(), id)
	if err != nil || len(resumed.Operations) != 1 || !resumed.Operations[0].State.IsValid() {
		t.Fatal("durable full record damaged", err)
	}
}

func TestSavedOperationCheckpointStillRejectsInvalidStateBeforePrivateReuse(t *testing.T) {
	directory := t.TempDir()
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := localfile.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("synthetic-invalid-checkpoint")
	for _, current := range []*localfile.Store{store, oracle} {
		if _, err = current.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	events := make(chan Event, 1)
	s := &Session{ID: id, store: store, operations: map[operation.ID]operation.Operation{}, subscribers: map[uint64]chan Event{1: events}}
	w := &serializedStore{s}
	initial := savedCheckpointFixture(jsontext.Value(`{"owner":true}`))
	savedCheckpointAdmit(t, store, id, initial)
	savedCheckpointAdmit(t, oracle, id, initial)
	if err = w.SaveOperation(t.Context(), id, initial); err != nil {
		t.Fatal(err)
	}
	<-events
	previous := s.operations[initial.ID]
	var invalid []operation.Operation
	for _, raw := range []jsontext.Value{
		jsontext.Value(`{"duplicate":1,"duplicate":2}`),
		jsontext.Value("\"\xff\""),
		jsontext.Value(strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000)),
	} {
		value := previous
		value.State = raw
		invalid = append(invalid, value)
	}
	for range 1 {
		value := previous
		value.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value(`"\xff"`)}
		invalid = append(invalid, value)
	}
	// Invalid metadata must fail at the unchanged complete canonical Save,
	// before the private post-validation clone helper can be reached.
	for _, value := range invalid {
		want := oracle.SaveOperation(t.Context(), id, value)
		got := w.SaveOperation(t.Context(), id, value)
		if want == nil || got == nil || reflect.TypeOf(got) != reflect.TypeOf(want) || got.Error() != want.Error() {
			t.Fatal("full Store validation changed", got, want)
		}
		if !reflect.DeepEqual(s.operations[initial.ID], previous) {
			t.Fatal("failed full Save changed private owner checkpoint")
		}
		select {
		case <-events:
			t.Fatal("failed full Save broadcast an operation")
		default:
		}
	}
}

func savedCheckpointCloneOutcome(value, previous operation.Operation, optimized bool) (result operation.Operation, failure any) {
	defer func() { failure = recover() }()
	if optimized {
		result = cloneSavedOperationCheckpoint(value, previous)
	} else {
		result = clone(value)
	}
	return result, nil
}

func TestSavedOperationChangedStateMatchesJSONCloneAndPanicOracle(t *testing.T) {
	previous := clone(savedCheckpointFixture(jsontext.Value(`{"prior":true}`)))
	states := []jsontext.Value{
		jsontext.Value(`{"literal":"日本語<>&\n\t\"quoted\"\\path","n":9007199254740993,"exp":1.2300e+2,"zero":-0}`),
		jsontext.Value(`{"literal":"\\u0041"}`),
		jsontext.Value(`null`), jsontext.Value(`[]`), jsontext.Value(`false`),
		jsontext.Value(" { \"nonminimal\" : \"\\/\\u0041\\u000a\" } "),
		jsontext.Value(`{"a":1,"a":2}`), jsontext.Value(`{"a":1,"\u0061":2}`),
		jsontext.Value("\"\xff\""), jsontext.Value(`{"unfinished":`),
	}
	for _, depth := range []int{64, 65, 9999, 10000} {
		states = append(states, jsontext.Value(strings.Repeat("[", depth)+"0"+strings.Repeat("]", depth)))
	}
	for i, raw := range states {
		t.Run(fmt.Sprintf("state%d", i), func(t *testing.T) {
			value := savedCheckpointFixture(raw)
			value.Execution = &operation.ExecutionMetadata{Dependencies: []operation.ID{"prior"}}
			value.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value(` {"key":"\u0041"} `)}
			want, wantFailure := savedCheckpointCloneOutcome(value, previous, false)
			got, gotFailure := savedCheckpointCloneOutcome(value, previous, true)
			if fmt.Sprintf("%T:%v", gotFailure, gotFailure) != fmt.Sprintf("%T:%v", wantFailure, wantFailure) || !reflect.DeepEqual(got, want) {
				t.Fatal("changed State differs from complete JSON clone or exact panic", gotFailure, wantFailure)
			}
		})
	}
}

func TestSavedOperationChangedStateDetachesIncomingAndPublicBytes(t *testing.T) {
	previous := clone(savedCheckpointFixture(jsontext.Value(`{"prior":true}`)))
	value := savedCheckpointFixture(jsontext.Value(`{"handle":"new independent result","nested":{"n":9007199254740993}}`))
	value.Execution = &operation.ExecutionMetadata{Dependencies: []operation.ID{"prior"}}
	value.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value(`{"key":3}`)}
	value.Permission = &permission.Decision{Mode: "default", State: "granted"}
	value.Denial = &permission.Error{Code: "denied", Capability: "synthetic", Reason: "test"}
	want := clone(value)
	got := cloneSavedOperationCheckpoint(value, previous)
	if &got.State[0] == &value.State[0] || &got.State[0] == &previous.State[0] {
		t.Fatal("changed State did not acquire independent owned bytes")
	}
	public := clone(Event{Kind: "operation", Operation: &got})
	value.State[0], previous.State[0] = '[', '['
	value.Execution.Dependencies[0] = "changed"
	value.Idempotence.Idempotency[0] = '['
	value.Permission.State, value.Denial.Reason = "changed", "changed"
	public.Operation.State[0] = '['
	if !reflect.DeepEqual(got, want) {
		t.Fatal("incoming, previous, metadata or public bytes aliased changed checkpoint")
	}
}

func TestSavedOperationChangedStateKeepsFullWireAndPublicDetachment(t *testing.T) {
	directory := t.TempDir()
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("synthetic-changed-checkpoint")
	if _, err = store.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 4)
	s := &Session{ID: id, store: store, operations: map[operation.ID]operation.Operation{}, subscribers: map[uint64]chan Event{1: events}}
	w := &serializedStore{s}
	initial := savedCheckpointFixture(jsontext.Value(`{"prior":true}`))
	savedCheckpointAdmit(t, store, id, initial)
	if err = w.SaveOperation(t.Context(), id, initial); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatal("initial checkpoint publication missing or duplicated")
	}
	<-events
	changed := initial
	changed.Status = operation.StatusCompleted
	changed.State = jsontext.Value(`{"handle":"changed terminal result","revision":9007199254740993}`)
	changed.Execution = &operation.ExecutionMetadata{Dependencies: []operation.ID{"prior"}}
	changed.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value(` {"key":"\u0041"} `)}
	want := clone(changed)
	if err = w.SaveOperation(t.Context(), id, changed); err != nil {
		t.Fatal(err)
	}
	if len(s.operations) != 1 || !reflect.DeepEqual(s.operations[changed.ID], want) || len(events) != 1 {
		t.Fatal("changed checkpoint was not saved and published exactly once")
	}
	public := <-events
	if public.Kind != "operation" || public.Operation == nil || !reflect.DeepEqual(*public.Operation, want) {
		t.Fatal("changed checkpoint public state differs")
	}
	encoded, err := os.ReadFile(filepath.Join(directory, string(id)+".session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(encoded, []byte{'\n'}), []byte{'\n'})
	wire, err := json.Marshal(struct {
		Type string                                  `json:"type"`
		Data struct{ Operation operation.Operation } `json:"data"`
	}{Type: "operation", Data: struct{ Operation operation.Operation }{changed}})
	if err != nil || !bytes.Equal(lines[len(lines)-1], wire) || bytes.Count(encoded, append(wire, '\n')) != 1 {
		t.Fatal("changed checkpoint lost its single complete canonical record", err)
	}
	view, err := s.Inspect(0, 1)
	if err != nil || len(view.Operations) != 1 || !reflect.DeepEqual(view.Operations[0], want) {
		t.Fatal("changed checkpoint public view differs", err)
	}
	for _, exposed := range []*operation.Operation{&changed, public.Operation, &view.Operations[0]} {
		exposed.State[0] = '['
		exposed.Execution.Dependencies[0] = "changed"
		exposed.Idempotence.Idempotency[0] = '['
	}
	if !reflect.DeepEqual(s.operations[initial.ID], want) || len(events) != 0 {
		t.Fatal("incoming/event/Inspect mutation changed owner or republished state")
	}
	resumed, err := store.Resume(t.Context(), id)
	if err != nil || len(resumed.Operations) != 1 || !reflect.DeepEqual(resumed.Operations[0], want) {
		t.Fatal("public mutation changed durable resumed checkpoint", err)
	}
}

func TestSavedOperationCheckpointMetadataErrorsKeepCompleteClonePanic(t *testing.T) {
	previous := clone(savedCheckpointFixture(jsontext.Value(`{"prior":true}`)))
	for _, equal := range []bool{false, true} {
		for _, field := range []string{"idempotency", "permission", "denial"} {
			t.Run(fmt.Sprintf("equal%v/%s", equal, field), func(t *testing.T) {
				value := savedCheckpointFixture(jsontext.Value(`{"changed":"` + strings.Repeat("x", 256) + `"}`))
				if equal {
					value.State = previous.State.Clone()
				}
				switch field {
				case "idempotency":
					// This flat field follows State, so omitting State changes the
					// JSON error offset unless the full clone is replayed.
					value.Idempotence = &operation.IdempotencyMetadata{Idempotency: jsontext.Value(`{"unfinished":`)}
				case "permission":
					value.Permission = &permission.Decision{Mode: "\xff"}
				case "denial":
					value.Denial = &permission.Error{Reason: "\xff"}
				}
				_, wantFailure := savedCheckpointCloneOutcome(value, previous, false)
				_, gotFailure := savedCheckpointCloneOutcome(value, previous, true)
				if wantFailure == nil || fmt.Sprintf("%T:%v", gotFailure, gotFailure) != fmt.Sprintf("%T:%v", wantFailure, wantFailure) {
					t.Fatal("metadata failure lost complete clone panic type/path/offset", gotFailure, wantFailure)
				}
			})
		}
	}
}
