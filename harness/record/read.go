package record

import (
	"errors"
	"time"
)

// ErrUnavailable means the canonical record references bytes that were never
// retained or that are missing. A bounded preview is not an original.
var ErrUnavailable = errors.New("original record bytes unavailable")

// Query filters using an inclusive From and an exclusive Before. Empty
// fields are unconstrained. Text is an exact, case-sensitive substring.
type Query struct {
	Kind   Kind
	Handle string
	From   time.Time
	Before time.Time
	Text   string
}

type Match struct {
	Locator Locator
	Kind    Kind
	Handle  string
	At      time.Time
	Excerpt []byte
}

// Page preserves raw bytes, including invalid UTF-8. Continue is a locator
// for the rest of the selected range and is empty when this is the last page.
type Page struct {
	Bytes    []byte
	Continue Locator
}

// Reference correlates observations without embedding their source text.
// Call IDs are scoped to a Turn, and operation and input IDs to a Session.
type Reference struct {
	Locator   Locator
	Turn      string `json:",omitzero"`
	Call      string `json:",omitzero"`
	Operation string `json:",omitzero"`
	Input     string `json:",omitzero"`
	Handle    string `json:",omitzero"`
}
