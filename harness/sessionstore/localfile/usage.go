package localfile

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/jsonspan"
)

// UsageCursor identifies a committed prefix, including across archival moves.
// A zero cursor reads the entire Session.
type UsageCursor struct {
	file   os.FileInfo
	offset int64
}

// UsageRecord is the metadata needed for a ledger. Response output, tool
// results and L2 state are deliberately excluded.
type UsageRecord struct {
	At         time.Time
	Kind       sessionstore.ItemKind
	Turn       session.Turn
	Response   *sessionstore.ModelResponse
	Host       *sessionstore.HostRecord
	Input      *inbox.Input
	Operations []operation.Operation
}

// ReadUsage streams a snapshot of the committed log and never acquires a
// writer lease. reset runs before the first record when the prefix changed.
func (s *Store) ReadUsage(ctx context.Context, id session.ID, cursor UsageCursor, reset func(), visit func(UsageRecord)) (UsageCursor, error) {
	f, err := s.OpenRecord(id)
	if err != nil {
		return cursor, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return cursor, err
	}
	if cursor.file == nil || !os.SameFile(cursor.file, info) || cursor.file.Size() > info.Size() ||
		cursor.file.Size() == info.Size() && !cursor.file.ModTime().Equal(info.ModTime()) {
		cursor = UsageCursor{}
		reset()
	}
	first := cursor.offset == 0
	input := bufio.NewReaderSize(io.NewSectionReader(f, cursor.offset, info.Size()-cursor.offset), 128<<10)
	for line, err := range committedLines(input) {
		if err != nil {
			return cursor, err
		}
		if err := context.Cause(ctx); err != nil {
			return cursor, err
		}
		if first {
			kind, value, err := decodeRecord(line)
			if err != nil || kind != recordSession {
				return cursor, fmt.Errorf("Session %q has no header", id)
			}
			header, err := decodeSessionHeader(value)
			if err != nil {
				return cursor, err
			}
			if header.ID != id {
				return cursor, fmt.Errorf("Session header identity mismatch for %q", id)
			}
			first = false
		} else {
			if err := projectUsageRecord(line, visit); err != nil {
				return cursor, fmt.Errorf("Session %q usage record at byte %d: %w", id, cursor.offset, err)
			}
		}
		cursor.offset += int64(len(line))
		cursor.file = info
	}
	if first {
		return cursor, fmt.Errorf("Session %q has no committed records", id)
	}
	return cursor, nil
}

func committedLines(input *bufio.Reader) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		var long []byte
		for {
			part, err := input.ReadSlice('\n')
			switch {
			case errors.Is(err, bufio.ErrBufferFull):
				long = append(long, part...)
				continue
			case errors.Is(err, io.EOF):
				return // the writer may still be appending the last record
			case err != nil:
				yield(nil, err)
				return
			}
			if len(long) != 0 {
				long = append(long, part...)
				part = long
			}
			if !yield(part, nil) {
				return
			}
			long = long[:0]
		}
	}
}

func projectUsageRecord(line []byte, visit func(UsageRecord)) error {
	// The writer puts Kind before Data and writes canonical records with no
	// whitespace. Avoid parsing large prompt/output records without evidence
	// for this projection. Other layouts use the typed decoder below.
	const itemPrefix = `{"type":"item","data":{"Item":{"Sequence":`
	const opPrefix = `{"type":"operation","data":{"Operation":`
	const startTool = `"SubagentStart"`
	if bytes.HasPrefix(line, []byte(opPrefix)) && !bytes.Contains(line, []byte(startTool)) &&
		!bytes.Contains(line, []byte(`"subagent"`)) {
		return nil
	}
	if bytes.HasPrefix(line, []byte(itemPrefix)) {
		prefix := line[:min(len(line), 180)]
		switch {
		case bytes.Contains(prefix, []byte(`,"Kind":"tool_call_status","Data":`)):
			if !bytes.Contains(line, []byte(startTool)) && !bytes.Contains(line, []byte(`"subagent"`)) {
				return nil
			}
		case bytes.Contains(prefix, []byte(`,"Kind":"context_state_event","Data":`)):
			return nil
		}
	}

	var row struct {
		Type recordType `json:"type"`
		Data struct {
			Item struct {
				RecordedAt time.Time
				Kind       sessionstore.ItemKind
				Data       jsonspan.Span
			}
			Operation  jsonspan.Span
			Operations []jsonspan.Span
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &row); err != nil {
		return err
	}
	i := row.Data.Item
	out := UsageRecord{At: i.RecordedAt, Kind: i.Kind}
	switch row.Type {
	case recordOperation:
		row.Data.Operations = append(row.Data.Operations, row.Data.Operation)
	case recordItem:
		data := i.Data.Value(line)
		switch i.Kind {
		case sessionstore.ItemTurn:
			if err := json.Unmarshal(data, &out.Turn); err != nil {
				return err
			}
		case sessionstore.ItemModelResponse:
			var rr struct {
				TurnID       session.TurnID
				AdapterError bool
				Response     struct {
					ID, Provider, Model string
					Stop                llm.StopReason
					Usage               llm.Usage
					Failure             *llm.Failure
				}
			}
			if err := json.Unmarshal(data, &rr); err != nil {
				return err
			}
			r := rr.Response
			out.Response = &sessionstore.ModelResponse{TurnID: rr.TurnID, AdapterError: rr.AdapterError,
				Response: llm.Response{ID: r.ID, Provider: r.Provider, Model: r.Model, Stop: r.Stop, Usage: r.Usage, Failure: r.Failure}}
		case sessionstore.ItemHostRecord:
			// The payloads of window and provider conversation records can
			// contain large snapshots. Read only the fields used here.
			var h struct {
				Kind          string
				Activity      *llm.ActivityRecord
				Configuration jsontext.Value
				Selection     *sessionstore.RuntimeSelection
			}
			if err := json.Unmarshal(data, &h); err != nil {
				return err
			}
			if h.Kind == "configuration" || h.Activity != nil || h.Selection != nil {
				out.Host = &sessionstore.HostRecord{Kind: h.Kind, Activity: h.Activity, Configuration: h.Configuration, Selection: h.Selection}
			}
		case sessionstore.ItemInput:
			if !bytes.Contains(data, []byte(`"budget-notice:`)) && !bytes.Contains(data, []byte(`"dispatch_operation"`)) {
				return nil
			}
			var input inbox.Input
			if err := json.Unmarshal(data, &input); err != nil {
				return err
			}
			out.Input = &input
		case sessionstore.ItemFork, sessionstore.ItemToolCallStatus:
		default:
			return nil
		}
	default:
		return fmt.Errorf("unexpected record type %q", row.Type)
	}
	for _, location := range row.Data.Operations {
		data := location.Value(line)
		if !bytes.Contains(data, []byte(`"SubagentStart"`)) && !bytes.Contains(data, []byte(`"subagent"`)) {
			continue
		}
		var op operation.Operation
		if err := json.Unmarshal(data, &op); err != nil {
			return err
		}
		if op.ToolName == "SubagentStart" || op.Type == operation.TypeRemoteJob {
			out.Operations = append(out.Operations, op)
		}
	}
	visit(out)
	return nil
}
