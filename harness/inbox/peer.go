package inbox

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

type CancelRequest struct{ OperationID string }

// PeerMessage is machine provenance, not a human instruction. The receiving
// transport must bind Sender/Target to its authenticated parent-child channel.
type PeerMessage struct {
	Version uint32
	Sender  string
	Target  string
	Handle  string
	Kind    string
	Text    string
}

func (p PeerMessage) Validate() error {
	if p.Version != 1 || p.Sender == "" || p.Target == "" || p.Sender == p.Target || p.Handle == "" || (p.Kind != "message" && p.Kind != "ready") || len(p.Text) > 32768 {
		return fmt.Errorf("invalid peer message")
	}
	return nil
}
func (input Input) DecodePeerMessage() (PeerMessage, error) {
	var p PeerMessage
	if input.Kind != InputPeer {
		return p, fmt.Errorf("not a peer input")
	}
	if err := json.Unmarshal(input.Payload, &p, json.RejectUnknownMembers(true)); err != nil {
		return p, fmt.Errorf("invalid peer payload")
	}
	return p, p.Validate()
}

// OperationIntent is a durable host command. Spec contains an inert operation
// plan; executors never run before this Input has committed.
type OperationIntent struct {
	OperationID string
	ToolName    string
	Spec        jsontext.Value
}
