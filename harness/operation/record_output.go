package operation

import "github.com/unreallabsai/unreal-agent/harness/record"

// RecordStreams lists the terminal evidence readable by RecordShow.
func RecordStreams(op Operation) []string {
	if op.Status != StatusCompleted && op.Status != StatusFailed && op.Status != StatusCanceled {
		return nil
	}
	switch op.Type {
	case TypeShell:
		state, err := DecodeShellState(op)
		if err == nil {
			var streams []string
			if state.OutPath != "" {
				streams = append(streams, record.Stdout)
			}
			if state.ErrPath != "" {
				streams = append(streams, record.Stderr)
			}
			if state.TerminalError != "" {
				streams = append(streams, record.Error)
			}
			return streams
		}
	case TypeRemoteJob:
		state, err := DecodeRemoteJobState(op)
		if err == nil {
			streams := []string{record.State}
			if state.TerminalResult != "" {
				streams = append(streams, record.Result)
			}
			if state.TerminalError != "" {
				streams = append(streams, record.Error)
			}
			return streams
		}
	case TypeValue:
		return []string{record.Result}
	}
	return []string{record.State}
}
