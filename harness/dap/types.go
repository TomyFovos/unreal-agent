// Package dap owns live debugger resources for one Host lifetime. Durable
// Operations contain typed requests/results, never live debugger process state.
package dap

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

const PlanType operation.RemoteJobPlanType = "dap"
const PlanVersion operation.RemoteJobPlanVersion = 1
const MaxFrameBytes = 1 << 20
const MaxResultBytes = 32 << 10
const MaxItems = 100

type Handle struct {
	ID         string `json:"id"`
	Generation string `json:"generation"`
}
type AdapterConfig struct {
	ID          string
	Path        string
	Arguments   []string
	Directory   string
	Environment []string
	// LaunchFields/AttachFields are trusted Host configuration for adapter-specific
	// options. Model requests cannot supply an opaque arbitrary adapter payload.
	LaunchFields      jsontext.Value
	AttachFields      jsontext.Value
	AllowedAttachPIDs []int
}
type Start struct {
	Adapter     string   `json:"adapter"`
	Program     string   `json:"program,omitzero"`
	Arguments   []string `json:"arguments,omitzero"`
	Directory   string   `json:"directory,omitzero"`
	ProcessID   int      `json:"process_id,omitzero"`
	StopOnEntry bool     `json:"stop_on_entry,omitzero"`
}
type Breakpoint struct {
	Line   int `json:"line"`
	Column int `json:"column,omitzero"`
}
type Request struct {
	Version            int          `json:"version"`
	OwnerGeneration    string       `json:"owner_generation"`
	Handle             Handle       `json:"handle"`
	Command            string       `json:"command"`
	Start              *Start       `json:"start,omitzero"`
	ThreadID           int          `json:"thread_id,omitzero"`
	FrameID            int          `json:"frame_id,omitzero"`
	VariablesReference int          `json:"variables_reference,omitzero"`
	StopEpoch          uint64       `json:"stop_epoch,omitzero"`
	Expression         string       `json:"expression,omitzero"`
	Source             string       `json:"source,omitzero"`
	Breakpoints        []Breakpoint `json:"breakpoints,omitzero"`
	Offset             int          `json:"offset,omitzero"`
	Count              int          `json:"count,omitzero"`
	Terminate          bool         `json:"terminate,omitzero"`
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func (r Request) Validate() error {
	if r.Version != 1 || !idPattern.MatchString(r.Handle.ID) || !idPattern.MatchString(r.OwnerGeneration) {
		return &Error{Code: "invalid_request"}
	}
	if r.Count < 0 || r.Count > MaxItems || r.Offset < 0 || len(r.Expression) > 8192 || len(r.Breakpoints) > MaxItems {
		return &Error{Code: "invalid_request"}
	}
	if r.Command == "launch" || r.Command == "attach" {
		if r.Start == nil || !idPattern.MatchString(r.Start.Adapter) || r.Handle.Generation != "" {
			return &Error{Code: "invalid_start"}
		}
		if r.Command == "launch" && (!filepath.IsAbs(r.Start.Program) || !filepath.IsAbs(r.Start.Directory) || r.Start.ProcessID != 0) {
			return &Error{Code: "invalid_launch"}
		}
		if r.Command == "attach" && (r.Start.ProcessID <= 0 || r.Start.Program != "" || r.Start.Directory != "" || len(r.Start.Arguments) != 0) {
			return &Error{Code: "invalid_attach"}
		}
		return nil
	}
	if r.Start != nil || r.Handle.Generation == "" {
		return &Error{Code: "invalid_handle"}
	}
	switch r.Command {
	case "threads", "inspect", "disconnect":
	case "continue", "pause", "next", "stepIn", "stepOut", "stackTrace":
		if r.ThreadID <= 0 {
			return &Error{Code: "invalid_thread"}
		}
	case "scopes":
		if r.FrameID <= 0 || r.StopEpoch == 0 {
			return &Error{Code: "invalid_frame"}
		}
	case "variables":
		if r.VariablesReference <= 0 || r.StopEpoch == 0 {
			return &Error{Code: "invalid_variables"}
		}
	case "evaluate":
		if r.Expression == "" || r.StopEpoch == 0 {
			return &Error{Code: "invalid_evaluation"}
		}
	case "setBreakpoints":
		if !filepath.IsAbs(r.Source) {
			return &Error{Code: "invalid_source"}
		}
		for _, b := range r.Breakpoints {
			if b.Line <= 0 || b.Column < 0 {
				return &Error{Code: "invalid_breakpoint"}
			}
		}
	default:
		return &Error{Code: "unsupported_command"}
	}
	return nil
}
func NewSpec(r Request) (operation.Spec, error) {
	if err := r.Validate(); err != nil {
		return operation.Spec{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxResultBytes {
		return operation.Spec{}, &Error{Code: "request_too_large"}
	}
	spec, err := operation.NewRemoteJobSpec(operation.RemoteJobPlan{Type: PlanType, Version: PlanVersion, Data: raw})
	spec.MaxOutputLength = MaxResultBytes
	return spec, err
}

type Error struct{ Code string }

func (e *Error) Error() string { return "dap: " + e.Code }
func codeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "adapter_failed"
}

// Item covers bounded DAP thread/frame/scope/variable/breakpoint records. Only
// normalized supported fields leave the transport; arbitrary response JSON does not.
type Item struct {
	ID                 int    `json:"id,omitzero"`
	Name               string `json:"name,omitzero"`
	Value              string `json:"value,omitzero"`
	Type               string `json:"type,omitzero"`
	VariablesReference int    `json:"variables_reference,omitzero"`
	Source             string `json:"source,omitzero"`
	Line               int    `json:"line,omitzero"`
	Column             int    `json:"column,omitzero"`
	Verified           bool   `json:"verified,omitzero"`
}
type Result struct {
	Version            int    `json:"version"`
	Handle             Handle `json:"handle"`
	Command            string `json:"command"`
	Status             string `json:"status"`
	StopEpoch          uint64 `json:"stop_epoch"`
	Items              []Item `json:"items,omitzero"`
	Value              string `json:"value,omitzero"`
	VariablesReference int    `json:"variables_reference,omitzero"`
	Truncated          bool   `json:"truncated,omitzero"`
	Error              string `json:"error,omitzero"`
}

func copyConfig(c AdapterConfig) AdapterConfig {
	c.Arguments = slices.Clone(c.Arguments)
	c.Environment = slices.Clone(c.Environment)
	c.AllowedAttachPIDs = slices.Clone(c.AllowedAttachPIDs)
	c.LaunchFields = slices.Clone(c.LaunchFields)
	c.AttachFields = slices.Clone(c.AttachFields)
	return c
}
