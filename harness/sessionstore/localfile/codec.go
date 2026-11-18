package localfile

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/jsonspan"
)

const formatVersion = 3

type recordType string

const (
	recordSession   recordType = "session"
	recordItem      recordType = "item"
	recordOperation recordType = "operation"
)

type logRecord struct {
	Type recordType     `json:"type"`
	Data jsontext.Value `json:"data"`
}

// recordValue encodes canonical values directly into their envelope. Keeping
// the raw envelope for alternate encoders avoids changing the persisted format.
type recordValue struct {
	Type recordType `json:"type"`
	Data any        `json:"data"`
}

type sessionRecord struct {
	Version int
	Session session.Session
}

type itemRecord struct {
	Item       sessionstore.Item
	Operations []operation.Operation `json:",omitempty"`
}

type operationRecord struct {
	Operation  operation.Operation
	RecordedAt time.Time `json:",omitzero"`
}

type decodedRecord struct {
	Type      recordType
	Item      itemRecord
	Operation operationRecord
	offset    int64
}

func encodeInitialLog(value session.Session, items []sessionstore.Item) ([]byte, error) {
	if err := validateSessionID(value.ID); err != nil {
		return nil, err
	}

	encoded, err := encodeRecord(recordSession, sessionRecord{
		Version: formatVersion,
		Session: value,
	})
	if err != nil {
		return nil, fmt.Errorf("encode session: %w", err)
	}

	for index, item := range items {
		if item.Sequence != sessionstore.Sequence(index+1) {
			return nil, fmt.Errorf("item %d has non-contiguous sequence %d", index, item.Sequence)
		}
		if item.RecordedAt.IsZero() {
			return nil, fmt.Errorf("item %d recorded time is zero", index)
		}

		record := itemRecord{Item: item}
		if item.Kind == sessionstore.ItemToolCallStatus {
			status := item.Data.(sessionstore.ToolCallStatus)
			record.Operations = status.Operations
			status.Operations = nil
			record.Item.Data = status
		}
		line, err := encodeRecord(recordItem, record)
		if err != nil {
			return nil, fmt.Errorf("encode item %d: %w", index, err)
		}
		encoded = append(encoded, line...)
	}
	return encoded, nil
}

func decodeLog(encoded []byte) (storedState, int64, error) {
	return replayLog(encoded, storedState{}, 0, nil)
}

func decodeRecord(line []byte) (recordType, jsontext.Value, error) {
	var record struct {
		Type recordType    `json:"type"`
		Data jsonspan.Span `json:"data"`
	}
	err := json.Unmarshal(line, &record)
	return record.Type, record.Data.Value(line), err
}

type historyItemJSON sessionstore.Item

// The writer emits Kind before Data. For the common large record, decode the
// typed response in the enclosing pass. The ordinary union decoder still
// handles reordered fields, legacy layouts and detailed validation errors.
func decodeResponseItem(line []byte) (itemRecord, bool) {
	if !bytes.HasPrefix(line, []byte(`{"type":"item","data":{"Item":{"Sequence":`)) ||
		!bytes.Contains(line[:min(len(line), 160)], []byte(`,"Kind":"model_response","Data":`)) {
		return itemRecord{}, false
	}
	var envelope struct {
		Type recordType `json:"type"`
		Data struct {
			Item struct {
				historyItemJSON
				Data *sessionstore.ModelResponse
			}
			Operations []operation.Operation
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil ||
		envelope.Type != recordItem || envelope.Data.Item.Kind != sessionstore.ItemModelResponse || envelope.Data.Item.Data == nil {
		return itemRecord{}, false
	}
	item := sessionstore.Item(envelope.Data.Item.historyItemJSON)
	item.Data = *envelope.Data.Item.Data
	return itemRecord{Item: item, Operations: envelope.Data.Operations}, true
}

func decodeSessionHeader(data jsontext.Value) (session.Session, error) {
	var header sessionRecord
	if err := json.Unmarshal(data, &header); err != nil {
		return session.Session{}, fmt.Errorf("decode session record: %w", err)
	}
	if header.Version == 1 {
		return session.Session{}, fmt.Errorf("legacy session format version 1 cannot be resumed")
	}
	if header.Version != formatVersion && header.Version != 2 {
		return session.Session{}, fmt.Errorf("unsupported session format version %d", header.Version)
	}
	if err := validateSessionID(header.Session.ID); err != nil {
		return session.Session{}, err
	}
	if header.Session.CreatedAt.IsZero() {
		return session.Session{}, fmt.Errorf("session %q creation time is zero", header.Session.ID)
	}
	return header.Session, nil
}

func replayLog(encoded []byte, value storedState, start int64, index *restoreIndex) (storedState, int64, error) {
	lastNewline := bytes.LastIndexByte(encoded, '\n')
	if lastNewline < 0 {
		return storedState{}, 0, fmt.Errorf("session log has no committed records")
	}
	committedSize := int64(lastNewline + 1)
	tail := encoded[start:committedSize]
	records := make([]decodedRecord, 0, bytes.Count(tail, []byte{'\n'}))
	itemCount := len(value.Items)
	inheritedItemCount := 0
	offset := start
	lineIndex := 0
	for line := range bytes.SplitSeq(tail, []byte{'\n'}) {
		recordOffset := offset
		if recordOffset == committedSize {
			break
		}
		offset += int64(len(line)) + 1
		lineIndex++
		atStart := recordOffset == 0
		itemValue, response := decodeResponseItem(line)
		kind := recordItem
		var data jsontext.Value
		if !response {
			var err error
			kind, data, err = decodeRecord(line)
			if err != nil {
				return storedState{}, 0, fmt.Errorf("decode record %d: %w", lineIndex, err)
			}
		}
		switch kind {
		case recordSession:
			if !atStart {
				return storedState{}, 0, fmt.Errorf("session record must be first")
			}
			header, err := decodeSessionHeader(data)
			if err != nil {
				return storedState{}, 0, err
			}
			value = newStoredState(header.ID, header.CreatedAt)
		case recordItem:
			if atStart {
				return storedState{}, 0, fmt.Errorf("session record must be first")
			}
			if !response {
				if err := json.Unmarshal(data, &itemValue); err != nil {
					return storedState{}, 0, fmt.Errorf("decode item record: %w", err)
				}
			}
			wantSequence := sessionstore.Sequence(itemCount + 1)
			if itemValue.Item.Sequence != wantSequence {
				return storedState{}, 0, fmt.Errorf(
					"item %d has sequence %d, want %d",
					itemCount,
					itemValue.Item.Sequence,
					wantSequence,
				)
			}
			if itemValue.Item.RecordedAt.IsZero() {
				return storedState{}, 0, fmt.Errorf("item %d recorded time is zero", itemCount)
			}
			if itemValue.Item.Kind != sessionstore.ItemToolCallStatus && len(itemValue.Operations) != 0 {
				return storedState{}, 0, fmt.Errorf(
					"item %d of kind %q initializes operations",
					itemCount,
					itemValue.Item.Kind,
				)
			}
			itemCount++
			if itemValue.Item.Kind == sessionstore.ItemFork {
				if start != 0 {
					return storedState{}, 0, fmt.Errorf("fork requires inherited history replay")
				}
				inheritedItemCount = itemCount
				if index != nil {
					index.inherited = wantSequence
				}
			}
			records = append(records, decodedRecord{Type: recordItem, Item: itemValue, offset: recordOffset})
		case recordOperation:
			if atStart {
				return storedState{}, 0, fmt.Errorf("session record must be first")
			}
			var update operationRecord
			if err := json.Unmarshal(data, &update); err != nil {
				return storedState{}, 0, fmt.Errorf("decode operation record: %w", err)
			}
			records = append(records, decodedRecord{
				Type: recordOperation, Operation: update, offset: recordOffset,
			})
		default:
			return storedState{}, 0, fmt.Errorf("unsupported record type %q", kind)
		}
	}

	replayedItems := len(value.Items)
	value.Items = slices.Grow(value.Items, itemCount-replayedItems)
	for recordIndex, record := range records {
		switch record.Type {
		case recordItem:
			itemIndex := replayedItems
			replayedItems++
			if itemIndex < inheritedItemCount {
				if len(record.Item.Operations) != 0 {
					return storedState{}, 0, fmt.Errorf("inherited item %d initializes operations", itemIndex)
				}
				value.inheritItem(record.Item.Item)
				if index != nil {
					index.items = append(index.items, record.offset)
				}
				continue
			}
			if err := replayItem(&value, record.Item); err != nil {
				return storedState{}, 0, fmt.Errorf("apply item %d: %w", itemIndex, err)
			}
			if err := value.observeItem(record.Item); err != nil {
				return storedState{}, 0, fmt.Errorf("observe item %d: %w", itemIndex, err)
			}
			if index != nil {
				index.addItem(record.offset, record.Item.Item.Sequence, len(value.Operations))
			}
		case recordOperation:
			if replayedItems < inheritedItemCount {
				return storedState{}, 0, fmt.Errorf(
					"operation record %d appears in inherited history",
					recordIndex+2,
				)
			}
			if err := value.applyOperationRecord(record.Operation, record.offset); err != nil {
				return storedState{}, 0, fmt.Errorf("apply operation record: %w", err)
			}
			if index != nil {
				index.operations[value.operationPositions[record.Operation.Operation.ID]] = operationLocation{Offset: record.offset}
				if index.scanOperations != nil {
					*index.scanOperations = append(*index.scanOperations, record.offset)
				}
			}
		}
	}
	return value, committedSize, nil
}

func replayItem(state *storedState, record itemRecord) error {
	switch record.Item.Kind {
	case sessionstore.ItemContextEvent:
		e := record.Item.Data.(contextstate.Event)
		if err := state.ContextProjection.Apply(e); err != nil {
			return err
		}
		state.Items = append(state.Items, state.appendItem(sessionstore.ItemContextEvent, e, record.Item.RecordedAt))
		return nil
	case sessionstore.ItemHostRecord:
		r := record.Item.Data.(sessionstore.HostRecord)
		item := state.sessionHead.appendItem(sessionstore.ItemHostRecord, r, record.Item.RecordedAt)
		state.Items = append(state.Items, item)
		return nil
	case sessionstore.ItemInput:
		return state.appendInput(record.Item.Data.(inbox.Input), record.Item.RecordedAt)
	case sessionstore.ItemTurn:
		return state.appendTurn(record.Item.Data.(session.Turn), record.Item.RecordedAt)
	case sessionstore.ItemModelResponse:
		return state.appendModelResponse(
			record.Item.Data.(sessionstore.ModelResponse),
			record.Item.RecordedAt,
		)
	case sessionstore.ItemToolCallStatus:
		return state.appendToolCallStatus(
			record.Item.Data.(sessionstore.ToolCallStatus),
			record.Operations,
			record.Item.RecordedAt,
		)
	default:
		return fmt.Errorf("unexpected owned item kind %q", record.Item.Kind)
	}
}

func encodeRecord(kind recordType, value any) ([]byte, error) {
	// These private record types contain the canonical values that we persist.
	// Marshal the complete envelope once so large values are not copied into a
	// raw JSON value and then validated and copied again. The full envelope still
	// receives JSON's normal validation, including its combined depth limit.
	switch value.(type) {
	case sessionRecord, itemRecord, operationRecord:
		encoded, err := json.Marshal(recordValue{Type: kind, Data: value})
		if err == nil {
			return finishRecord(encoded)
		}
		// Preserve the established validation error's context and precedence.
	}

	// Arbitrary values retain their standalone marshal context. A custom
	// marshaler may observe its encoder's depth or JSON pointer.
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(logRecord{Type: kind, Data: data})
	if err != nil {
		return nil, err
	}
	return finishRecord(encoded)
}

func finishRecord(encoded []byte) ([]byte, error) {
	if bytes.IndexByte(encoded, '\n') >= 0 {
		return nil, fmt.Errorf("encoded record contains a newline")
	}
	return append(encoded, '\n'), nil
}
