package jsonoptions

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

// PreserveNumbers keeps numeric values decoded into any as json.Number.
// Concrete numeric fields retain their declared Go types.
func PreserveNumbers() json.Options {
	return json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, value *any) error {
		if dec.PeekKind() != jsontext.KindNumber {
			return errors.ErrUnsupported
		}
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		*value = jsonv1.Number(raw)
		return nil
	}))
}
