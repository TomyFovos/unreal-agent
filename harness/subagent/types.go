// Package subagent composes separate agent processes using durable operations
// and parent-child Inbox channels. It does not own another scheduler or task DB.
package subagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"io"
	"path/filepath"
)

const PlanType operation.RemoteJobPlanType = "subagent"
const PlanVersion operation.RemoteJobPlanVersion = 1

// Template is resolved by the Host, not supplied by model arguments. Runtime is
// non-secret versioned provider/profile selection and configuration references.
type Template struct {
	Workspace              string
	MutationStateDirectory string `json:",omitzero"`
	Runtime                jsontext.Value
	// Policy is the child's ceiling. With permission modes the child follows
	// its parent's mode: Baseline is what it may do in Read Only and without
	// approval in Ask, Policy what Full Access and approvals allow.
	Policy   permission.Config
	Baseline *permission.Config `json:",omitzero"`
	// Default is the runtime a start without one uses, instead of the
	// parent's current selection.
	Default *RuntimeRequest `json:",omitzero"`
	// Subagents are the workers this child may start itself: one level, and
	// never at ultra effort.
	Subagents map[string]Template `json:",omitzero"`
}
type Plan struct {
	Version       uint32
	Action        string
	ParentID      session.ID
	ChildID       session.ID                 `json:",omitzero"`
	Template      string                     `json:",omitzero"`
	Configuration *Template                  `json:",omitzero"`
	Handle        operation.ID               `json:",omitzero"`
	Text          string                     `json:",omitzero"`
	Result        *sessionstore.FinishResult `json:",omitzero"`
	RuntimeBound  bool                       `json:",omitzero"`
	// Request and Allow answer a child's approval request.
	Request string `json:",omitzero"`
	Allow   bool   `json:",omitzero"`
	// Notify tells the parent when the child ends, as a message that wakes
	// it. A child started without a tool call (the subagent command) has no
	// tool result to do that.
	Notify bool `json:",omitzero"`
}
type ChildConfig struct {
	Version                uint32
	ParentID               session.ID
	ChildID                session.ID
	OperationID            operation.ID
	SessionDirectory       string
	Workspace              string
	Runtime                jsontext.Value
	Policy                 permission.Config
	ReadyID                inbox.ID
	Task                   string
	MutationStateDirectory string                        `json:",omitzero"`
	ProjectInstructions    *projectinstructions.Snapshot `json:",omitzero"`
	Baseline               *permission.Config            `json:",omitzero"`
	Subagents              map[string]Template           `json:",omitzero"`
	// A text-only child completes from a canonical model response, without a
	// synthetic tool call. ProviderProcess authorizes only its isolated CLI.
	TextOnly        bool `json:",omitzero"`
	ProviderProcess bool `json:",omitzero"`
}
type Handle struct {
	Version     uint32
	ParentID    session.ID
	ChildID     session.ID
	OperationID operation.ID
	Finish      *sessionstore.FinishRecord `json:",omitzero"`
	// Approvals are the child's requests waiting for this parent (U17).
	Approvals []Approval `json:",omitzero"`
}

// Approval is a child's request for its parent agent to decide (U17).
type Approval struct {
	ID    string
	Tool  string `json:",omitzero"`
	Scope string `json:",omitzero"`
	Allow bool   `json:",omitzero"`
}
type Sender func(context.Context, inbox.Input) (host.Receipt, error)

// ChildLink is what a child runtime gets from its live parent besides the
// recorded configuration: the channel back, and the parent's current
// permission mode (empty when the parent has no modes).
type ChildLink struct {
	Send Sender
	Mode permission.Mode
}
type ChildFactory func(context.Context, ChildConfig, ChildLink) (*host.Session, io.Closer, error)

func ChildID(parent session.ID, id operation.ID) session.ID {
	sum := sha256.Sum256([]byte("unreal-agent/subagent/v1\x00" + string(parent) + "\x00" + string(id)))
	return session.ID("child-" + hex.EncodeToString(sum[:16]))
}
func (p Plan) Validate() error {
	if p.Version != 1 || p.ParentID == "" || len(p.Text) > 32768 {
		return fmt.Errorf("invalid subagent plan")
	}
	switch p.Action {
	case "start":
		if p.Template == "" || p.Configuration == nil || p.Text == "" || !filepath.IsAbs(p.Configuration.Workspace) || !p.Configuration.Runtime.IsValid() {
			return fmt.Errorf("invalid child configuration")
		}
	case "send", "cancel":
		if p.Handle == "" {
			return fmt.Errorf("child operation handle is required")
		}
	case "approve":
		if p.Handle == "" || p.Request == "" || len(p.Request) > 256 {
			return fmt.Errorf("child operation handle and request are required")
		}
	case "parent":
		if p.Text == "" {
			return fmt.Errorf("empty parent message")
		}
	case "finish":
		if p.Result == nil {
			return fmt.Errorf("Finish result is required")
		}
		return p.Result.Validate()
	default:
		return fmt.Errorf("unsupported subagent action")
	}
	return nil
}
func NewSpec(plan Plan) (operation.Spec, error) {
	if err := plan.Validate(); err != nil {
		return operation.Spec{}, err
	}
	data, err := json.Marshal(plan)
	if err != nil {
		return operation.Spec{}, err
	}
	return operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: PlanType, Version: PlanVersion, Data: data})
}
func DecodePlan(op operation.Operation) (Plan, error) {
	state, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		return Plan{}, err
	}
	if state.Plan.Type != PlanType || state.Plan.Version != PlanVersion {
		return Plan{}, operation.ErrUnsupported
	}
	var p Plan
	if err = json.Unmarshal(state.Plan.Data, &p, json.RejectUnknownMembers(true)); err != nil {
		return p, fmt.Errorf("invalid subagent plan encoding")
	}
	if err = p.Validate(); err != nil {
		return p, err
	}
	if p.Action == "start" {
		id := ChildID(p.ParentID, op.ID)
		if p.ChildID != "" && p.ChildID != id {
			return p, fmt.Errorf("child identity mismatch")
		}
		p.ChildID = id
	}
	return p, nil
}
func DecodeHandle(op operation.Operation) (Handle, error) {
	s, err := operation.DecodeRemoteJobState(op)
	if err != nil {
		return Handle{}, err
	}
	var h Handle
	err = json.Unmarshal(s.Handle, &h, json.RejectUnknownMembers(true))
	if err != nil || h.Version != 1 || h.OperationID != op.ID || h.ParentID == "" || h.ChildID != ChildID(h.ParentID, op.ID) {
		return Handle{}, fmt.Errorf("invalid subagent handle")
	}
	if h.Finish != nil {
		err = h.Finish.Validate()
	}
	return h, err
}
func (c ChildConfig) Validate() error {
	if c.Version != 1 || c.ParentID == "" || c.ChildID != ChildID(c.ParentID, c.OperationID) || c.OperationID == "" || c.ReadyID != inbox.ID("ready:"+string(c.OperationID)) || !filepath.IsAbs(c.SessionDirectory) || !filepath.IsAbs(c.Workspace) || !c.Runtime.IsValid() || c.Task == "" || len(c.Task) > 32768 {
		return fmt.Errorf("invalid child handshake configuration")
	}
	if c.MutationStateDirectory != "" && !filepath.IsAbs(c.MutationStateDirectory) {
		return fmt.Errorf("child mutation state directory must be absolute")
	}
	if c.ProjectInstructions != nil {
		if err := c.ProjectInstructions.Validate(); err != nil {
			return err
		}
	}
	// This implementation offers bounded built-in file/network executors, not an
	// arbitrary subprocess sandbox. No child can request ambient capabilities.
	canFinish := false
	for _, name := range c.Policy.Tools {
		if name == "Finish" {
			canFinish = true
		}
	}
	if !canFinish && !c.TextOnly {
		return fmt.Errorf("child policy must enable Finish")
	}
	if c.TextOnly || c.ProviderProcess {
		s := sessionstore.SelectionFromConfiguration(c.Runtime)
		if s == nil || s.Provider != "claude-code" || !c.ProviderProcess {
			return fmt.Errorf("invalid text-only child transport")
		}
		if c.TextOnly == sessionstore.ToolBridgeEnabledFromConfiguration(c.Runtime) {
			return fmt.Errorf("child transport capability disagrees with runtime")
		}
	}
	// Children are workers: they run commands within their policy (D1).
	return ValidateWorkers(c.Subagents)
}

// ValidateWorkers checks the templates a child may start: workers that start
// no children of their own, never at ultra effort.
func ValidateWorkers(templates map[string]Template) error {
	for name, t := range templates {
		if name == "" || len(name) > 128 {
			return fmt.Errorf("invalid worker template name")
		}
		if len(t.Subagents) != 0 {
			return fmt.Errorf("worker template %q cannot start children", name)
		}
		s := sessionstore.SelectionFromConfiguration(t.Runtime)
		if s == nil {
			return fmt.Errorf("worker template %q has no readable runtime", name)
		}
		if s.Effort == llm.ReasoningEffortUltra || t.Default != nil && t.Default.Effort == llm.ReasoningEffortUltra {
			return fmt.Errorf("worker template %q cannot use ultra effort", name)
		}
	}
	return nil
}
func (c ChildConfig) InitialInput() inbox.Input {
	data, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(c.ParentID), Target: string(c.ChildID), Handle: string(c.OperationID), Kind: "message", Text: c.Task})
	return inbox.Input{ID: inbox.ID("task:" + string(c.OperationID)), Kind: inbox.InputPeer, Payload: data}
}
