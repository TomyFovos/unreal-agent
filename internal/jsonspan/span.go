// Package jsonspan finds a raw value within a byte slice passed to json.Unmarshal.
package jsonspan

import "encoding/json/jsontext"

// Span records the position of a JSON value without copying it. Use it only
// when the original input bytes are available for a subsequent typed decode.
type Span struct {
	start, end int
}

func (s *Span) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	value, err := dec.ReadValue()
	if err != nil {
		return err
	}
	s.end = int(dec.InputOffset())
	s.start = s.end - len(value)
	return nil
}

func (s Span) Value(input []byte) jsontext.Value {
	return input[s.start:s.end]
}
