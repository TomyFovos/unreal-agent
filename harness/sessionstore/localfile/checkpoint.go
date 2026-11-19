package localfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

const (
	restoreCheckpointVersion  = 5
	restoreCheckpointInterval = 1024
)

// A checkpoint stores the write head and positions in an already validated
// prefix. Transcript and Operation payloads are read from the canonical file.
// Bump the version when replay changes how the write head is derived.
type restoreCheckpoint struct {
	Version      int
	SessionID    session.ID
	Size         int64
	PrefixHash   []byte
	Inherited    sessionstore.Sequence
	Items        []int64
	Operations   []operationLocation
	LatestTurn   session.TurnID
	Turns        map[session.TurnID]struct{}
	OwnedTurns   map[session.TurnID]struct{}
	Responded    map[session.TurnID]struct{}
	ToolStatuses []checkpointToolCall
	Context      contextstate.Projection `json:",omitzero"`
	ContextFacts contextFacts            `json:",omitzero"`
}

type checkpointToolCall struct {
	TurnID session.TurnID
	CallID string
}

// Sequence is zero for a standalone operation record at Offset. Otherwise
// Index identifies the initializing operation in that canonical Item.
type operationLocation struct {
	Sequence sessionstore.Sequence
	Index    int
	Offset   int64
}

// restoreIndex is updated only by the Session's serialized writer, after
// canonical appends have committed. It never owns or duplicates payloads.
type restoreIndex struct {
	items          []int64
	operations     []operationLocation
	inherited      sessionstore.Sequence
	prefix         hash.Hash
	savedSize      int64
	appends        int
	scanOperations *[]int64                   // populated only on explicit record reads
	contextFacts   *sessionstore.ContextFacts // populated only on analysis reads
}

func (i *restoreIndex) addItem(offset int64, sequence sessionstore.Sequence, operationCount int) {
	i.items = append(i.items, offset)
	first := len(i.operations)
	for n := first; n < operationCount; n++ {
		i.operations = append(i.operations, operationLocation{Sequence: sequence, Index: n - first})
	}
}

func (s *Store) checkpointPath(id session.ID) string {
	return filepath.Join(s.directory, string(id)+".restore-checkpoint")
}

// SaveRestoreCheckpoint publishes the current validated prefix while the
// caller holds the writer lease. A cache write error has no canonical effect.
func (s *Store) SaveRestoreCheckpoint(id session.ID) error {
	s.writeStateCacheMutex.Lock()
	cached := s.writeStateCache[id]
	s.writeStateCacheMutex.Unlock()
	i := cached.restoreIndex
	if i == nil || cached.committedSize == i.savedSize {
		return nil
	}
	i.appends = 0
	h := cached.head
	c := restoreCheckpoint{
		Version: restoreCheckpointVersion, SessionID: id, Size: cached.committedSize, PrefixHash: i.prefix.Sum(nil),
		Inherited: i.inherited, Items: i.items, Operations: i.operations, LatestTurn: h.latestTurnID,
		Turns: h.turns, OwnedTurns: h.ownedTurns, Responded: h.respondedTurns,
		ToolStatuses: make([]checkpointToolCall, 0, len(h.toolCallStatuses)),
		Context:      h.ContextProjection,
		ContextFacts: h.ContextFacts,
	}
	for k := range h.toolCallStatuses {
		c.ToolStatuses = append(c.ToolStatuses, checkpointToolCall{TurnID: k.turnID, CallID: k.callID})
	}
	data, err := encodeRestoreCheckpoint(c)
	if err != nil {
		return err
	}
	if err := publishFile(s.directory, s.checkpointPath(id), data); err != nil {
		return err
	}
	i.savedSize = c.Size
	return nil
}

func encodeRestoreCheckpoint(c restoreCheckpoint) ([]byte, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return append(data, sum[:]...), nil
}

func (s *Store) readRestoreCheckpoint(id session.ID, encoded []byte) (storedState, int64, *restoreIndex, bool) {
	data, err := os.ReadFile(s.checkpointPath(id))
	if err != nil || len(data) <= sha256.Size {
		return storedState{}, 0, nil, false
	}
	body, checksum := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	sum := sha256.Sum256(body)
	var c restoreCheckpoint
	if !bytes.Equal(sum[:], checksum) || json.Unmarshal(body, &c) != nil ||
		c.Version != restoreCheckpointVersion || c.SessionID != id ||
		c.Size <= 0 || c.Size > int64(len(encoded)) || encoded[c.Size-1] != '\n' {
		return storedState{}, 0, nil, false
	}

	prefix := sha256.New()
	prefix.Write(encoded[:c.Size])
	if !bytes.Equal(c.PrefixHash, prefix.Sum(nil)) {
		return storedState{}, 0, nil, false
	}
	state, err := c.materialize(encoded[:c.Size])
	if err != nil {
		return storedState{}, 0, nil, false
	}
	i := &restoreIndex{items: c.Items, operations: c.Operations, inherited: c.Inherited, prefix: prefix, savedSize: c.Size}
	return state, c.Size, i, true
}

func (c restoreCheckpoint) materialize(prefix []byte) (storedState, error) {
	kind, data, err := checkpointRecord(prefix, 0)
	if err != nil || kind != recordSession {
		return storedState{}, fmt.Errorf("restore checkpoint has no session header")
	}
	header, err := decodeSessionHeader(data)
	if err != nil || header.ID != c.SessionID {
		return storedState{}, fmt.Errorf("restore checkpoint session mismatch")
	}
	if c.Inherited > sessionstore.Sequence(len(c.Items)) || c.Turns == nil || c.OwnedTurns == nil || c.Responded == nil {
		return storedState{}, fmt.Errorf("restore checkpoint has an invalid head")
	}
	state := newStoredState(header.ID, header.CreatedAt)
	state.ContextProjection = c.Context
	state.ContextFacts = c.ContextFacts
	state.latestTurnID, state.itemSequence = c.LatestTurn, sessionstore.Sequence(len(c.Items))
	state.turns, state.ownedTurns, state.respondedTurns = c.Turns, c.OwnedTurns, c.Responded
	for _, call := range c.ToolStatuses {
		state.toolCallStatuses[toolCallStatusKey{turnID: call.TurnID, callID: call.CallID}] = struct{}{}
	}

	state.Items = slices.Grow(state.Items, len(c.Items))
	var previous int64
	for n, offset := range c.Items {
		if offset <= previous {
			return storedState{}, fmt.Errorf("restore checkpoint item offsets are unordered")
		}
		previous = offset
		line, err := checkpointLine(prefix, offset)
		if err != nil {
			return storedState{}, err
		}
		record, response := decodeResponseItem(line)
		if !response {
			kind, data, err := decodeRecord(line)
			if err != nil || kind != recordItem || json.Unmarshal(data, &record) != nil {
				return storedState{}, fmt.Errorf("restore checkpoint item %d is invalid", n+1)
			}
		}
		if record.Item.Sequence != sessionstore.Sequence(n+1) {
			return storedState{}, fmt.Errorf("restore checkpoint item %d is invalid", n+1)
		}
		if status, ok := record.Item.Data.(sessionstore.ToolCallStatus); ok {
			status.Operations = nil
			if record.Item.Sequence > c.Inherited {
				status.Operations = record.Operations
			}
			record.Item.Data = status
		}
		state.Items = append(state.Items, record.Item)
	}
	state.Operations = slices.Grow(state.Operations, len(c.Operations))
	for _, location := range c.Operations {
		op, err := location.load(prefix, state.Items)
		if err != nil || validateOperation(op) != nil {
			return storedState{}, fmt.Errorf("restore checkpoint operation is invalid")
		}
		if _, exists := state.operationPositions[op.ID]; exists {
			return storedState{}, fmt.Errorf("restore checkpoint repeats an operation")
		}
		state.operationPositions[op.ID] = len(state.Operations)
		state.Operations = append(state.Operations, op)
	}
	return state, nil
}

func checkpointRecord(prefix []byte, offset int64) (recordType, []byte, error) {
	line, err := checkpointLine(prefix, offset)
	if err != nil {
		return "", nil, err
	}
	return decodeRecord(line)
}

func checkpointLine(prefix []byte, offset int64) ([]byte, error) {
	if offset < 0 || offset >= int64(len(prefix)) || offset > 0 && prefix[offset-1] != '\n' {
		return nil, fmt.Errorf("offset is outside the committed record boundaries")
	}
	line, _, _ := bytes.Cut(prefix[offset:], []byte{'\n'})
	return line, nil
}

func (l operationLocation) load(prefix []byte, items []sessionstore.Item) (operation.Operation, error) {
	if l.Sequence == 0 {
		kind, data, err := checkpointRecord(prefix, l.Offset)
		var record operationRecord
		if err == nil && kind == recordOperation && json.Unmarshal(data, &record) == nil {
			return record.Operation, nil
		}
	} else if l.Sequence <= sessionstore.Sequence(len(items)) {
		switch value := items[l.Sequence-1].Data.(type) {
		case inbox.Input:
			return sessionstore.DecodeOperationIntent(value)
		case sessionstore.ToolCallStatus:
			if l.Index >= 0 && l.Index < len(value.Operations) {
				return value.Operations[l.Index], nil
			}
		}
	}
	return operation.Operation{}, fmt.Errorf("invalid operation location")
}
