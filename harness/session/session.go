// Package session defines durable session and turn identity.
package session

import (
	"time"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

type ID string

type TurnID string

type TurnType string

const (
	TurnRegular    TurnType = "regular"
	TurnCompaction TurnType = "compaction"
)

type Session struct {
	ID        ID
	CreatedAt time.Time
}

type Turn struct {
	ID             TurnID
	PreviousTurnID TurnID
	Type           TurnType
	// References a canonical runtime selection; zero denotes creation identity.
	RuntimeRevision  uint64            `json:",omitzero"`
	ContextAdmission *ContextAdmission `json:",omitzero"`
	ContextStrategy  string            `json:",omitzero"`
	// A continuation remains inside the original model request. Its input
	// watermark excludes queued inputs until a provider delivers them at a
	// tool batch boundary. ExternalInputWatermark also preserves foreground vs
	// background scheduling after restoration.
	ToolContinuation       TurnID `json:",omitzero"`
	InputWatermark         int    `json:",omitzero"`
	ExternalInputWatermark int    `json:",omitzero"`
	// BatchInputs projects the queued inbox inputs at this continuation.
	// The matching model response is the acknowledgement of delivery.
	BatchInputs bool `json:",omitzero"`
	// Canonical inputs offered at this mid-turn read boundary. The next
	// response still acknowledges delivery through the original watermark.
	BatchRecords []record.Locator `json:",omitzero"`
}

// ContextAdmission records the pre-send decision for this Turn, so its estimate
// can be compared with the same Turn's reported response input after replay.
// All sizes except RequestBytes are tokens. No prompt bodies are retained here.
type ContextAdmission struct {
	InputTokens, EstimatedInputTokens, MeasuredPrefixTokens, EstimatedDeltaTokens int64
	BudgetTokens, WindowTokens                                                    int64
	RequestBytes                                                                  int64
	Measurement, Estimator, WindowSource, HeaderFingerprint                       string
	WindowBoundary                                                                record.Locator `json:",omitzero"` // first request in a rebuilt window
}
