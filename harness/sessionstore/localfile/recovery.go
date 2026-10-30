package localfile

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// RecoveryOperations is an eligibility scan for stopped-session cleanup. JSON's
// decoder skips response bodies, context and other unrelated values without
// constructing transcript items. A caller must read the canonical snapshot
// before making any recovery writes. Unrecognized data uses the canonical path.
func (s *Store) RecoveryOperations(ctx context.Context, id session.ID) ([]operation.Operation, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	encoded, err := os.ReadFile(s.sessionPath(id))
	if err != nil {
		return nil, fmt.Errorf("read session %q: %w", id, err)
	}
	ops, ok := recoveryOperations(ctx, encoded)
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if !ok {
		state, _, err := decodeLog(encoded)
		if err != nil {
			return nil, fmt.Errorf("read session %q: %w", id, err)
		}
		return state.Operations, nil
	}
	return ops, nil
}

func recoveryOperations(ctx context.Context, encoded []byte) ([]operation.Operation, bool) {
	end := bytes.LastIndexByte(encoded, '\n')
	if end < 0 {
		return nil, false
	}
	// The fields required to decide whether any abandoned child needs work.
	// All remaining JSON is still syntax-checked by the standard decoder.
	type checkpoint struct {
		ID       operation.ID
		Type     operation.Type
		ToolName string
		Status   operation.Status
	}
	var latest = make(map[operation.ID]operation.Operation)
	seenCalls := make(map[struct {
		turn session.TurnID
		call string
	}]bool)
	for line := range bytes.SplitSeq(encoded[:end], []byte{'\n'}) {
		if context.Cause(ctx) != nil {
			return nil, false
		}
		var record struct {
			Type recordType `json:"type"`
			Data struct {
				Version int
				Item    struct {
					Kind sessionstore.ItemKind
					Data struct {
						Kind   string
						TurnID session.TurnID
						CallID string
					}
				}
				Operations []checkpoint
				Operation  checkpoint
			} `json:"data"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, false
		}
		set := func(op checkpoint) {
			latest[op.ID] = operation.Operation{ID: op.ID, Type: op.Type, ToolName: op.ToolName, Status: op.Status}
		}
		switch record.Type {
		case recordSession:
			if record.Data.Version != 2 && record.Data.Version != formatVersion {
				return nil, false
			}
		case recordOperation:
			set(record.Data.Operation)
		case recordItem:
			switch record.Data.Item.Kind {
			case sessionstore.ItemFork:
				clear(latest)
				clear(seenCalls)
			case sessionstore.ItemInput:
				if record.Data.Item.Data.Kind != string(inbox.InputControl) {
					continue
				}
				var full struct {
					Data struct{ Item sessionstore.Item } `json:"data"`
				}
				if err := json.Unmarshal(line, &full); err != nil {
					return nil, false
				}
				if op, err := sessionstore.DecodeOperationIntent(full.Data.Item.Data.(inbox.Input)); err == nil {
					latest[op.ID] = op
				}
			case sessionstore.ItemToolCallStatus:
				key := struct {
					turn session.TurnID
					call string
				}{record.Data.Item.Data.TurnID, record.Data.Item.Data.CallID}
				if seenCalls[key] {
					continue
				}
				seenCalls[key] = true
				for _, op := range record.Data.Operations {
					set(op)
				}
			}
		default:
			return nil, false
		}
	}
	ops := make([]operation.Operation, 0, len(latest))
	for _, op := range latest {
		ops = append(ops, op)
	}
	return ops, true
}
