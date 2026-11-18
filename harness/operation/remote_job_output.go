package operation

func prepareRemoteJobOutput(current Operation, state *RemoteJobState) {
	prepareOutput(&state.TerminalResult, &state.ResultBytes, &state.ResultTruncated, &state.OriginalResult, current.MaxOutputLength)
	prepareOutput(&state.TerminalError, &state.ErrorBytes, &state.ErrorTruncated, &state.OriginalError, current.MaxOutputLength)
}

func prepareOutput(text *string, size *int, truncated *bool, original *[]byte, limit int) {
	if *truncated && *text != "" {
		return
	}
	raw := *text
	if len(*original) > 0 && raw == sanitizeUTF8(string(*original)) {
		raw = string(*original)
	}
	*size = len(raw)
	*text, *truncated = BoundOutput(raw, limit)
	*original = originalBytes(raw, *text)
}

func originalBytes(original, shown string) []byte {
	if original == shown {
		return nil
	}
	return []byte(original)
}
