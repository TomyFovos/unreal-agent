package inbox

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/record"
)

type CancelRequest struct {
	OperationID string
	By          string `json:",omitzero"`
}

// PeerMessage is machine provenance, not a human instruction. The receiving
// transport must bind Sender/Target to its authenticated parent-child channel.
type PeerMessage struct {
	Version uint32
	Sender  string
	Target  string
	Handle  string
	Kind    string
	Text    string
	// RuleID records how a forwarded question was routed. Text stays verbatim.
	RuleID string `json:"rule_id,omitzero"`
	// ReportIncluded distinguishes a single ended notice with a full Finish
	// from historical text that sometimes bundled several child reports.
	ReportIncluded bool           `json:",omitzero"`
	ReportLocator  record.Locator `json:",omitzero"`
}

func (p PeerMessage) Validate() error {
	if p.Version != 1 || p.Sender == "" || p.Target == "" || p.Sender == p.Target || p.Handle == "" || (p.Kind != "message" && p.Kind != "progress" && p.Kind != "ready" && p.Kind != "ended" && p.Kind != "budget") {
		return fmt.Errorf("invalid peer message")
	}
	if p.ReportLocator != "" {
		a, err := p.ReportLocator.Address()
		if err != nil || p.Kind != "ended" || a.Kind != record.ChildReport || a.Session != p.Sender || a.Handle != p.Handle {
			return fmt.Errorf("invalid peer report locator")
		}
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
