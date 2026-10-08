// Package modelcatalog reads nonsecret model metadata owned by external CLIs.
// It never fetches models, reads credentials, or manufactures fallback lists.
package modelcatalog

import (
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type Model struct {
	ID, Name           string
	ResolvedModel      string `json:",omitzero"`
	Description        string `json:",omitzero"`
	SupportsEffort     *bool  `json:",omitzero"`
	AdaptiveThinking   *bool  `json:",omitzero"`
	FastMode           *bool  `json:",omitzero"`
	Efforts            []llm.ReasoningEffort
	DefaultEffort      llm.ReasoningEffort
	ContextWindow      int64
	UnsupportedEfforts bool
}
type Catalog struct {
	Models        []Model
	Available     bool
	Source        string
	FetchedAt     time.Time
	Problem       string
	Authoritative bool `json:",omitzero"`
}

// Provider namespaces catalogs: model aliases are not globally unique.
type Provider struct {
	ID, Name, Availability string
	Tools                  bool
	Catalog                Catalog
	ToolBridge             string `json:",omitzero"`
}

// Capabilities describe a live registration, not a canonical permission grant.
type Capabilities struct {
	Tools      bool
	ToolBridge string `json:",omitzero"` // available, blocked_by_policy, disabled, unknown
}

func ProviderName(id string) string {
	switch id {
	case "openai-codex":
		return "OpenAI Codex"
	case "claude-code":
		return "Claude Code"
	default:
		return id
	}
}

// Matches keeps the picker alias separate from its advertised wire identity.
func (m Model) Matches(id string) bool {
	return m.ID == id || m.ResolvedModel != "" && m.ResolvedModel == id
}

func (m Model) AllowsEffort(e llm.ReasoningEffort) bool {
	if m.SupportsEffort != nil && !*m.SupportsEffort {
		return e == ""
	}
	for _, supported := range m.Efforts {
		if supported == e {
			return true
		}
	}
	return false
}

// Clone keeps UI-local appends/navigation from mutating the shared read cache.
func (c Catalog) Clone() Catalog {
	c.Models = append([]Model(nil), c.Models...)
	for i := range c.Models {
		c.Models[i].Efforts = append([]llm.ReasoningEffort(nil), c.Models[i].Efforts...)
		for _, field := range []**bool{&c.Models[i].SupportsEffort, &c.Models[i].AdaptiveThinking, &c.Models[i].FastMode} {
			if *field != nil {
				v := **field
				*field = &v
			}
		}
	}
	return c
}

func (c Catalog) FindSelection(id string) (Model, error) {
	if m, err := c.Find(id); err == nil {
		return m, nil
	}
	for _, m := range c.Models {
		if m.Matches(id) {
			return m, nil
		}
	}
	return Model{}, errors.New("model is not in the available catalog")
}

// ReadCodexCache accepts only the CLI's explicit list visibility and supported
// reasoning metadata. Identity, instructions and other cache fields are ignored.
func ReadCodexCache(path string) Catalog {
	c := Catalog{Source: path, Problem: "catalog unavailable"}
	// Reject nonregular sources before opening them (a FIFO must not block a
	// picker), and recheck the descriptor before reading bounded metadata.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return c
	}
	f, err := os.Open(path)
	if err != nil {
		return c
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return c
	}
	b, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil || len(b) > 16<<20 {
		return c
	}
	var raw struct {
		FetchedAt time.Time `json:"fetched_at"`
		Models    []struct {
			ID         string              `json:"slug"`
			Name       string              `json:"display_name"`
			Visibility string              `json:"visibility"`
			Priority   int                 `json:"priority"`
			Default    llm.ReasoningEffort `json:"default_reasoning_level"`
			Levels     []struct {
				Effort llm.ReasoningEffort `json:"effort"`
			} `json:"supported_reasoning_levels"`
			Context int64 `json:"context_window"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return c
	}
	sort.SliceStable(raw.Models, func(i, j int) bool { return raw.Models[i].Priority < raw.Models[j].Priority })
	seen := map[string]bool{}
	for _, m := range raw.Models {
		if m.Visibility != "list" {
			continue
		}
		if !validID(m.ID) || seen[m.ID] {
			return Catalog{Source: path, Problem: "catalog unavailable"}
		}
		seen[m.ID] = true
		model := Model{ID: m.ID, Name: m.Name, DefaultEffort: m.Default, ContextWindow: max(0, m.Context)}
		if model.Name == "" {
			model.Name = model.ID
		}
		efforts := map[llm.ReasoningEffort]bool{}
		for _, level := range m.Levels {
			if !level.Effort.Valid() {
				model.UnsupportedEfforts = true
			}
			// Intersect metadata with efforts the existing harness can encode.
			if level.Effort.Valid() && !efforts[level.Effort] {
				model.Efforts = append(model.Efforts, level.Effort)
				efforts[level.Effort] = true
			}
		}
		if len(model.Efforts) > 0 {
			c.Models = append(c.Models, model)
		}
	}
	if len(c.Models) == 0 {
		return Catalog{Source: path, Problem: "catalog unavailable"}
	}
	c.Available, c.Problem, c.FetchedAt = true, "", raw.FetchedAt
	return c
}
func validID(id string) bool {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, " \t\n\r") {
		return false
	}
	for _, r := range id {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}
func (c Catalog) Find(id string) (Model, error) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, nil
		}
	}
	return Model{}, errors.New("model is not in the available catalog")
}
