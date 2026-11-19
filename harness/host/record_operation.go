package host

import (
	"context"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"golang.org/x/sys/unix"
)

func operationRecordHandle(op operation.Operation, fallback operation.ID) operation.ID {
	if p, ok := childArchivePlan(op); ok {
		if p.Action == "start" {
			return op.ID
		}
		if p.Handle != "" {
			return p.Handle
		}
	}
	return fallback
}

func (s *Session) operationRecordContent(ctx context.Context, owner session.ID, op operation.Operation, stream string) (recordContent, error) {
	if stream == record.State {
		return memoryRecord(op.State), nil
	}
	switch op.Type {
	case operation.TypeShell:
		state, err := operation.DecodeShellState(op)
		if err != nil {
			return recordContent{}, err
		}
		var path string
		size := int64(-1)
		switch stream {
		case record.Stdout:
			path = state.OutPath
			if state.Result != nil {
				size = state.Result.OutSize
			}
		case record.Stderr:
			path = state.ErrPath
			if state.Result != nil {
				size = state.Result.ErrSize
			}
		case record.Error:
			return originalOutput(state.TerminalError, state.OriginalError, state.ErrorTruncated)
		}
		if path != "" {
			file, err := s.store.OpenOperationCapture(ctx, owner, path, unix.O_NOFOLLOW|unix.O_NONBLOCK)
			if err != nil {
				return recordContent{}, originalUnavailable(err)
			}
			return fileRecord(file, size)
		}
	case operation.TypeRemoteJob:
		state, err := operation.DecodeRemoteJobState(op)
		if err != nil {
			return recordContent{}, err
		}
		switch stream {
		case record.Result:
			return originalOutput(state.TerminalResult, state.OriginalResult, state.ResultTruncated)
		case record.Error:
			return originalOutput(state.TerminalError, state.OriginalError, state.ErrorTruncated)
		}
	case operation.TypeValue:
		if stream == record.Result {
			value, err := operation.DecodeValue(op)
			return memoryRecord(value), err
		}
	}
	return recordContent{}, fmt.Errorf("unknown output stream %q for operation %q", stream, op.ID)
}

func originalOutput(shown string, original []byte, truncated bool) (recordContent, error) {
	if len(original) != 0 {
		return memoryRecord(original), nil
	}
	if truncated {
		return recordContent{}, record.ErrUnavailable
	}
	return memoryRecord([]byte(shown)), nil
}
