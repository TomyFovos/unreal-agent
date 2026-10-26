// Package modelcatalog reads nonsecret model metadata owned by external CLIs.
// It never performs inference, decodes credentials, or manufactures model lists.
package modelcatalog

import (
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"slices"
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
	// No verified effort metadata: omit the optional request feature rather
	// than claiming support or inventing an effort value.
	EffortUnknown bool `json:",omitzero"`
	// MultiAgentEffort is what Codex sends when ultra is selected.
	MultiAgentEffort llm.ReasoningEffort `json:",omitzero"`
}

// RequestEffort is the effort a Codex request carries for the selected one.
// Codex never sends ultra: ultra selects automatic delegation and sends the
// model's multi-agent effort, else max, else the highest other level, else
// medium (codex-rs resolve_reasoning_effort).
func (m Model) RequestEffort(selected llm.ReasoningEffort) llm.ReasoningEffort {
	if selected != llm.ReasoningEffortUltra {
		return selected
	}
	if m.MultiAgentEffort.Valid() && m.MultiAgentEffort != llm.ReasoningEffortUltra {
		return m.MultiAgentEffort
	}
	if slices.Contains(m.Efforts, llm.ReasoningEffortMax) {
		return llm.ReasoningEffortMax
	}
	for i := len(m.Efforts) - 1; i >= 0; i-- {
		if m.Efforts[i] != llm.ReasoningEffortUltra {
			return m.Efforts[i]
		}
	}
	return llm.ReasoningEffortMedium
}

type Catalog struct {
	Models        []Model
	Available     bool
	Source        string
	FetchedAt     time.Time
	Problem       string
	ProblemCode   string `json:",omitzero"` // closed, nonsecret catalog failure reason
	Authoritative bool   `json:",omitzero"`
}

// Provider namespaces catalogs: model aliases are not globally unique.
type Provider struct {
	ID, Name, Availability string
	Tools                  bool
	Reason                 AvailabilityReason `json:",omitzero"`
	Catalog                Catalog
	ToolBridge             string `json:",omitzero"`
}

// AvailabilityReason is deliberately independent of provider error text.
type AvailabilityReason string

const (
	AuthRequired         AvailabilityReason = "auth_required"
	CLIMissing           AvailabilityReason = "cli_missing_or_unsupported"
	ManagedTrustRequired AvailabilityReason = "managed_trust_required"
	ProxyInvalid         AvailabilityReason = "docker_proxy_invalid"
	CatalogUnavailable   AvailabilityReason = "catalog_unavailable"
	ProviderDisabled     AvailabilityReason = "operator_disabled"
	ProviderCheckFailed  AvailabilityReason = "provider_check_failed"
)

func (r AvailabilityReason) Hint() string {
	switch r {
	case AuthRequired:
		return "sign in with the external CLI"
	case CLIMissing:
		return "install a supported external CLI"
	case ManagedTrustRequired:
		return "configure providers: audit policy and explicitly approve trust"
	case ProxyInvalid:
		return "verify the selected Docker OAuth source and HTTPS proxy"
	case CatalogUnavailable:
		return "refresh the external CLI model catalog"
	case ProviderDisabled:
		return "configure providers to enable this provider"
	case ProviderCheckFailed:
		return "run provider preflight; no fallback"
	default:
		return ""
	}
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
	case "claude-native":
		return "Claude Native"
	case "claude":
		return "Claude"
	default:
		return id
	}
}

// Matches keeps the picker alias separate from its advertised wire identity.
func (m Model) Matches(id string) bool {
	return m.ID == id || m.ResolvedModel != "" && m.ResolvedModel == id
}

func (m Model) AllowsEffort(e llm.ReasoningEffort) bool {
	if m.EffortUnknown {
		return e == ""
	}
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
	fail := func(code string) Catalog {
		return Catalog{Source: path, Problem: "catalog unavailable", ProblemCode: code}
	}
	// Reject nonregular sources before opening them (a FIFO must not block a
	// picker), and recheck the descriptor before reading bounded metadata.
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fail("codex_cache_missing")
	}
	if err != nil || !info.Mode().IsRegular() {
		return fail("codex_cache_unreadable")
	}
	before := info
	f, err := os.Open(path)
	if err != nil {
		return fail("codex_cache_unreadable")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !os.SameFile(before, info) || !info.Mode().IsRegular() {
		return fail("codex_cache_unreadable")
	}
	if info.Size() > 16<<20 {
		return fail("codex_cache_oversized")
	}
	b, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil || len(b) > 16<<20 {
		return fail("codex_cache_oversized")
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
			Context    int64               `json:"context_window"`
			MultiAgent llm.ReasoningEffort `json:"multi_agent_reasoning_effort"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return fail("codex_cache_invalid")
	}
	sort.SliceStable(raw.Models, func(i, j int) bool { return raw.Models[i].Priority < raw.Models[j].Priority })
	seen := map[string]bool{}
	for _, m := range raw.Models {
		if m.Visibility != "list" {
			continue
		}
		if !validID(m.ID) || seen[m.ID] {
			return fail("codex_cache_invalid")
		}
		seen[m.ID] = true
		model := Model{ID: m.ID, Name: m.Name, DefaultEffort: m.Default, ContextWindow: max(0, m.Context)}
		if m.MultiAgent.Valid() {
			model.MultiAgentEffort = m.MultiAgent
		}
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
		return fail("codex_cache_no_selectable_models")
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
