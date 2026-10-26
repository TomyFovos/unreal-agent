package host

import (
	"bytes"
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

// cloneSavedOperationCheckpoint runs only after the concrete local Store has
// saved the complete canonical Operation. The prior State is Session-owned and
// immutable; public views/events still clone it. Restored opaque State may be
// unnormalized, so the lexical gate below conservatively retains the old clone.
func cloneSavedOperationCheckpoint(value, previous operation.Operation) operation.Operation {
	if value.ID == "" || value.ID != previous.ID || value.Type != previous.Type || value.Version != previous.Version || len(value.State) == 0 || !unchangedJSONFormatting(value.State) {
		return clone(value)
	}
	reuseState := bytes.Equal(value.State, previous.State)
	if !reuseState && (!value.State.IsValid() || !shallowSavedOperationState(value.State)) {
		return clone(value)
	}
	// Retain the old JSON normalization and pointer detachment for all metadata.
	// If omitting State changes a metadata error's offset or precedence, replay
	// the complete old clone so even its panic remains the original one.
	metadata := value
	metadata.State = nil
	data, err := json.Marshal(metadata)
	if err != nil {
		return clone(value)
	}
	var result operation.Operation
	if err = json.Unmarshal(data, &result); err != nil {
		return clone(value)
	}
	if reuseState {
		// The equal prior State already has exact normalization and ownership.
		result.State = previous.State
	} else {
		// Changed State needs its own bytes, but not a full JSON round trip.
		result.State = value.State.Clone()
	}
	return result
}

// Changed State is optimized only well below JSON's nesting limit, leaving
// room for both the Operation and complete canonical record envelopes. This
// is an eligibility bound, not an input limit: deeper values use the old clone.
// IsValid and unchangedJSONFormatting are checked before this lexical scan.
func shallowSavedOperationState(raw []byte) bool {
	const maxDepth = 64
	depth, inString := 0, false
	for i := 0; i < len(raw); i++ {
		if inString {
			switch raw[i] {
			case '\\':
				i++
			case '"':
				inString = false
			}
			continue
		}
		switch raw[i] {
		case '"':
			inString = true
		case '[', '{':
			depth++
			if depth > maxDepth {
				return false
			}
		case ']', '}':
			depth--
		}
	}
	return true
}

// This is only an eligibility test after full canonical validation, never a
// replacement JSON validator. Without nonminimal escapes or insignificant
// whitespace, json/v2's default raw-value formatting preserves every byte:
// raw numbers/object order stay unchanged and UTF-8 strings are already minimal.
// Only the standard shortest two-byte escapes are accepted; every \u escape
// and escaped solidus conservatively retains the old normalization path.
func unchangedJSONFormatting(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch c {
		case '\\':
			if !inString || i+1 == len(raw) {
				return false
			}
			i++
			switch raw[i] {
			case '"', '\\', 'b', 'f', 'n', 'r', 't':
			default:
				return false
			}
		case '"':
			inString = !inString
		case ' ', '\t', '\r', '\n':
			if !inString {
				return false
			}
		}
	}
	return !inString
}
