package sessionstore

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"strings"
)

// SelectionFromConfiguration extracts only public model metadata from the
// immutable creation identity; instructions and auth fields are never returned.
func SelectionFromConfiguration(raw jsontext.Value) *RuntimeSelection {
	v := metadataFromConfiguration(raw)
	if v == nil {
		return nil
	}
	s := &RuntimeSelection{Version: 1, Provider: v.Provider.Provider, Model: v.Provider.Model.ID, Name: v.Provider.Model.ID, Effort: v.ReasoningEffort}
	if s.Validate() != nil {
		return nil
	}
	return s
}

// ManagedPolicyModeFromConfiguration projects only the explicit Claude policy
// boundary. An omitted mode is reject; unknown values are never reported trusted.
// This projection does not authorize Claude or expose managed settings contents.
func ManagedPolicyModeFromConfiguration(raw jsontext.Value) string {
	v := metadataFromConfiguration(raw)
	if v == nil || v.Provider.Provider != "claude-code" {
		return ""
	}
	var claude struct {
		ManagedPolicyMode string `json:"managedPolicyMode"`
	}
	if len(v.ClaudeCode) > 0 && json.Unmarshal(v.ClaudeCode, &claude) != nil {
		return ""
	}
	switch claude.ManagedPolicyMode {
	case "", "reject":
		return "reject"
	case "trust":
		return "trust"
	default:
		return ""
	}
}

// ToolBridgeEnabledFromConfiguration extracts only explicit public opt-in. It
// neither reads Claude policy/credentials nor confers execution permission.
func ToolBridgeEnabledFromConfiguration(raw jsontext.Value) bool {
	v := metadataFromConfiguration(raw)
	if v == nil || v.Provider.Provider != "claude-code" {
		return false
	}
	var c struct{ ToolBridge struct{ Enabled bool } }
	return json.Unmarshal(v.ClaudeCode, &c) == nil && c.ToolBridge.Enabled
}

// Only this closed operator mode is exported. Neither protocol JSON nor
// credentials/policy bodies are decoded into the read-only projection.
func ToolBridgeModeFromConfiguration(raw jsontext.Value) string {
	v := metadataFromConfiguration(raw)
	if v == nil || v.Provider.Provider != "claude-code" {
		return ""
	}
	var c struct {
		ToolBridge struct {
			Enabled bool
			Mode    string
		}
	}
	if json.Unmarshal(v.ClaudeCode, &c) != nil {
		return ""
	}
	if !c.ToolBridge.Enabled {
		return "disabled"
	}
	switch c.ToolBridge.Mode {
	case "structured":
		return "structured"
	case "", "mcp":
		return "mcp"
	}
	return ""
}

type configurationMetadata struct {
	Runtime  jsontext.Value
	Provider struct {
		Provider string `json:"provider"`
		Model    struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	ReasoningEffort llm.ReasoningEffort
	ClaudeCode      jsontext.Value
}

func metadataFromConfiguration(raw jsontext.Value) *configurationMetadata {
	// Parent subagent identities and delegated ChildConfig both wrap the same
	// runtime under Runtime. Inspect that known field only, with a depth bound;
	// the surrounding templates, task, policy and credential references remain
	// outside this projection.
	for depth := 0; depth < 3; depth++ {
		var v configurationMetadata
		if json.Unmarshal(raw, &v) != nil {
			return nil
		}
		if v.Provider.Model.ID != "" {
			return &v
		}
		if len(v.Runtime) == 0 {
			return nil
		}
		raw = v.Runtime
	}
	return nil
}

// RuntimeSelection is a versioned, nonsecret provider/model/effort choice.
// Credential material and permissions remain outside this selection; Binding
// records an operator backend reference/configuration without rewriting creation.
type RuntimeSelection struct {
	Version       uint32
	Revision      uint64
	RequestID     string
	Provider      string
	Model         string
	Name          string
	Effort        llm.ReasoningEffort
	ContextWindow int64 `json:",omitzero"`
	// Binding is a nonsecret operator backend snapshot. It binds a newly
	// selected provider without changing the immutable creation configuration.
	Binding string `json:",omitzero"`
}

func (s RuntimeSelection) Validate() error {
	if len(s.Binding) > 128<<10 || s.Binding != "" && !jsontext.Value(s.Binding).IsValid() {
		return errors.New("invalid provider binding")
	}
	if s.Version != 1 || s.Provider == "" || s.Model == "" || len(s.Model) > 128 || len(s.Name) > 256 || s.ContextWindow < 0 || !s.Effort.Valid() && !(s.Provider == "claude-code" && s.Effort == "") {
		return errors.New("invalid runtime selection")
	}
	for _, value := range []string{s.Provider, s.Model, s.RequestID, s.Name} {
		if len(value) > 256 || strings.ContainsAny(value, "\x00\x1b\r\n") {
			return errors.New("invalid runtime selection")
		}
	}
	return nil
}

const (
	HostRuntimeSelection = "runtime_selection"
	HostRuntimeApplied   = "runtime_applied"
)
