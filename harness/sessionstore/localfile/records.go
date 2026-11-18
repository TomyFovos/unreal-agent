package localfile

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// RecordLog is an on-demand view of a single committed prefix. It owns its
// data and uses the same replay and record positions as restore. Raw items
// are slices of the original log, including the line terminators.
type RecordLog struct {
	ID          session.ID
	Items       []sessionstore.Item
	Operations  []operation.Operation
	encoded     []byte
	index       restoreIndex
	positions   map[operation.ID]int
	checkpoints []int64
}

func (s *Store) ReadRecordLog(ctx context.Context, id session.ID) (*RecordLog, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	encoded, err := s.readRecord(id)
	if err != nil {
		return nil, err
	}
	var checkpoints []int64
	index := restoreIndex{scanOperations: &checkpoints}
	state, size, err := replayLog(encoded, storedState{}, 0, &index)
	if err != nil {
		return nil, err
	}
	if state.Snapshot.Session.ID != id {
		return nil, fmt.Errorf("record log session identity mismatch")
	}
	return &RecordLog{ID: id, Items: state.Items, Operations: state.Operations,
		encoded: encoded[:size], index: index, positions: state.operationPositions, checkpoints: checkpoints}, nil
}

func (l *RecordLog) RawItem(seq sessionstore.Sequence) []byte {
	if seq == 0 || seq > sessionstore.Sequence(len(l.Items)) {
		return nil
	}
	line := l.encoded[l.index.items[seq-1]:]
	return line[:bytes.IndexByte(line, '\n')+1]
}

func (l *RecordLog) Operation(id operation.ID) (operation.Operation, bool) {
	i, ok := l.positions[id]
	if !ok {
		return operation.Operation{}, false
	}
	return l.Operations[i], true
}

func (l *RecordLog) OperationTime(i int) time.Time {
	location := l.index.operations[i]
	if location.Sequence != 0 {
		return l.Items[location.Sequence-1].RecordedAt
	}
	// Older standalone operation checkpoints did not record a timestamp.
	_, data, _ := checkpointRecord(l.encoded, location.Offset)
	var metadata struct{ RecordedAt time.Time }
	_ = json.Unmarshal(data, &metadata)
	return metadata.RecordedAt
}

type OperationEvidence struct {
	Address record.Address
	At      time.Time
	Bytes   []byte
}

func (l *RecordLog) WalkOperationEvidence(yield func(OperationEvidence) error) error {
	for _, offset := range l.checkpoints {
		line, err := checkpointLine(l.encoded, offset)
		if err != nil {
			return err
		}
		_, data, err := decodeRecord(line)
		if err != nil {
			return err
		}
		var metadata struct {
			Operation  struct{ ID operation.ID }
			RecordedAt time.Time
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			return err
		}
		a := record.Address{Kind: record.OperationCheckpoint, Session: string(l.ID), Operation: string(metadata.Operation.ID), Offset: offset}
		if err := yield(OperationEvidence{Address: a, At: metadata.RecordedAt, Bytes: l.encoded[offset : offset+int64(len(line))+1]}); err != nil {
			return err
		}
	}
	return nil
}

func (l *RecordLog) RawOperation(id operation.ID, offset int64) ([]byte, error) {
	line, err := checkpointLine(l.encoded, offset)
	if err != nil {
		return nil, err
	}
	kind, data, err := decodeRecord(line)
	var metadata struct{ Operation struct{ ID operation.ID } }
	if err != nil || kind != recordOperation || json.Unmarshal(data, &metadata) != nil || metadata.Operation.ID != id {
		return nil, fs.ErrNotExist
	}
	return l.encoded[offset : offset+int64(len(line))+1], nil
}

// openSessionSidecar opens a trusted namespace under the same archive
// transaction as OpenRecord. The caller authorizes the logical source first.
func (s *Store) openSessionSidecar(id session.ID, relative string, flags int) (*os.File, error) {
	open := func(root string) (*os.File, error) {
		dir, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		defer dir.Close()
		return dir.OpenFile(relative, os.O_RDONLY|flags, 0)
	}
	file, err := open(s.directory)
	if !errors.Is(err, fs.ErrNotExist) {
		return file, err
	}
	return s.openArchivedSidecar(id, relative, flags)
}

func (s *Store) openArchivedSidecar(id session.ID, relative string, flags int) (*os.File, error) {
	entry, err := s.ArchiveIndex(id)
	if err != nil {
		return nil, err
	}
	dir, err := os.OpenRoot(s.archiveChildDirectory(entry))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.OpenFile(relative, os.O_RDONLY|flags, 0)
}

// OpenOperationCapture applies ordinary read permission to the recorded path.
// A path in the managed operations namespace may have moved with its Session.
func (s *Store) OpenOperationCapture(ctx context.Context, id session.ID, path string, flags int) (*os.File, error) {
	relative, err := filepath.Rel(s.directory, path)
	managed := filepath.Join("operations", string(id))
	if err != nil || !filepath.IsLocal(relative) || filepath.Dir(filepath.Dir(relative)) != managed {
		return openReadFile(ctx, path, flags)
	}
	file, err := openReadFile(ctx, path, flags)
	if !errors.Is(err, fs.ErrNotExist) {
		return file, err
	}
	return s.openArchivedSidecar(id, relative, flags)
}
