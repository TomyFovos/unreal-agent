// Package session defines durable session and turn identity.
package session

import "time"

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
	RuntimeRevision uint64 `json:",omitzero"`
	// A continuation remains inside the original model request. Its input
	// watermark excludes queued inputs until a provider delivers them at a
	// steering boundary. ExternalInputWatermark also preserves foreground vs
	// background scheduling after restoration.
	ToolContinuation       TurnID `json:",omitzero"`
	InputWatermark         int    `json:",omitzero"`
	ExternalInputWatermark int    `json:",omitzero"`
}
