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
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
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
	Workspace string
	Runtime   jsontext.Value
	Policy    permission.Config
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
}
type ChildConfig struct {
	Version          uint32
	ParentID         session.ID
	ChildID          session.ID
	OperationID      operation.ID
	SessionDirectory string
	Workspace        string
	Runtime          jsontext.Value
	Policy           permission.Config
	ReadyID          inbox.ID
	Task             string
}
type Handle struct {
	Version     uint32
	ParentID    session.ID
	ChildID     session.ID
	OperationID operation.ID
	Finish      *sessionstore.FinishRecord `json:",omitzero"`
}
type Sender func(context.Context, inbox.Input) (host.Receipt, error)
type ChildFactory func(context.Context, ChildConfig, Sender) (*host.Session, io.Closer, error)

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
	// This implementation offers bounded built-in file/network executors, not an
	// arbitrary subprocess sandbox. No child can request ambient capabilities.
	if c.Policy.ProcessMode != permission.ProcessDenied || c.Policy.FilesystemUnrestricted || c.Policy.NetworkUnrestricted {
		return &permission.Error{Code: permission.Unsupported, Capability: "child process", Reason: "bounded child executors required"}
	}
	return nil
}
func (c ChildConfig) InitialInput() inbox.Input {
	data, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(c.ParentID), Target: string(c.ChildID), Handle: string(c.OperationID), Kind: "message", Text: c.Task})
	return inbox.Input{ID: inbox.ID("task:" + string(c.OperationID)), Kind: inbox.InputPeer, Payload: data}
}
