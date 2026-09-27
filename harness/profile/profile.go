// Package profile provides versioned, I/O-free prompt composition. It has no
// access to credentials, tool registration, permission or lifecycle controls.
package profile

import (
	"errors"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

type Selection struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Source  string `json:"source"`
}

func Default() Selection {
	return Selection{ID: "conservative", Version: 1, Source: "configured default"}
}

type Resolved struct {
	selection Selection
	addition  string
	guidance  map[string]string
}

func (r Resolved) Selection() Selection { return r.selection }
func Resolve(s Selection, p provider.Selection) (Resolved, error) {
	if s.Version != 1 {
		return Resolved{}, errors.New("profile: unsupported_version")
	}
	if strings.TrimSpace(s.Source) == "" {
		return Resolved{}, errors.New("profile: selection_source_required")
	}
	r := Resolved{selection: s, guidance: map[string]string{}}
	switch s.ID {
	case "conservative":
		r.addition = "Use observed file contents and current revisions when editing. Keep changes focused and report uncertainty explicitly."
	case "openai-reasoning":
		if (p.Provider != "openai" && p.Provider != "openai-codex") || p.Model.Family != "openai-reasoning" {
			return Resolved{}, errors.New("profile: incompatible_model_family")
		}
		r.addition = "Inspect relevant code before editing. Prefer a concise execution plan and concrete changes; summarize results and unresolved uncertainty."
		r.guidance["read"] = "Read the relevant range and retain its revision for a subsequent edit."
		r.guidance["edit"] = "Use the revision from the read that informed this edit. If it is stale, read the affected code again."
		r.guidance["write"] = "Supply the expected current revision, including an explicit absent-file precondition for creation."
	default:
		return Resolved{}, errors.New("profile: unsupported_id")
	}
	return r, nil
}
func ValidateResume(recorded, requested Selection) error {
	if recorded != requested {
		return errors.New("profile: incompatible_resume_selection")
	}
	return nil
}

// Compose only adjusts existing descriptions. It cannot register a tool, change
// a schema or control response/lifecycle semantics. Call once at composition.
func (r Resolved) Compose(system string, tools []llm.Tool) (string, []llm.Tool) {
	copied := slices.Clone(tools)
	for i := range copied {
		if guidance := r.guidance[copied[i].Name]; guidance != "" {
			copied[i].Description = strings.TrimSpace(copied[i].Description + "\n" + guidance)
		}
	}
	return strings.TrimSpace(system + "\n\n" + r.addition), copied
}
