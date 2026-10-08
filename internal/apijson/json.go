// Package apijson supplies the JSON defaults used by generated API types.
package apijson

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"io"

	"github.com/unreallabsai/unreal-agent/internal/jsonoptions"
)

type RawMessage = jsonv1.RawMessage
type Number = jsonv1.Number

func NewDecoder(reader io.Reader) *jsonv1.Decoder {
	decoder := jsonv1.NewDecoder(reader)
	decoder.UseNumber()
	return decoder
}

func Valid(data []byte) bool {
	return jsonv1.Valid(data)
}

func Marshal(value any) ([]byte, error) {
	return json.Marshal(value, json.Deterministic(true))
}

func Unmarshal(data []byte, value any) error {
	return json.Unmarshal(data, value, jsonoptions.PreserveNumbers())
}
