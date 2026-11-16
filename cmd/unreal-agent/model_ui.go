package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/opencodego"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

type modelRequest struct {
	Action         string
	ID             session.ID
	Generation     string
	Expected       uint64
	Selection      sessionstore.RuntimeSelection
	Refresh        bool                     `json:",omitzero"`
	Provider       string                   `json:",omitzero"`
	InputID        inbox.ID                 `json:",omitzero"`
	Template, Task string                   `json:",omitzero"`
	ChildRuntime   *subagent.RuntimeRequest `json:",omitzero"`
	HostBinding    *launcherHostBinding     `json:",omitzero"`
}
type modelReply struct {
	Catalog      *modelcatalog.Catalog                `json:",omitzero"`
	Providers    []modelcatalog.Provider              `json:",omitzero"`
	Error        string                               `json:",omitzero"`
	Hint         string                               `json:",omitzero"`
	Receipt      *host.Receipt                        `json:",omitzero"`
	Capabilities map[string]modelcatalog.Capabilities `json:",omitzero"`
	// SubagentModel is /model-sub's choice; Pending means it applies once the
	// Host starts again, because this one has no sub-agents.
	SubagentModel *sessionstore.RuntimeSelection `json:",omitzero"`
	Pending       bool                           `json:",omitzero"`
}
type modelUI struct {
	apiReads   map[string]func(context.Context, bool) (modelcatalog.Catalog, error)
	rediscover func(context.Context) error
	supported  []modelcatalog.Provider
	binding    *launcherHostBinding
	owner      *host.Host
	// claudeAgent reads the Claude route's catalog (U21).
	claudeAgent *claudeCatalog
	codexRead   func(context.Context, bool) (modelcatalog.Catalog, error)
	next        http.Handler
	runtime     agentrunner.RuntimeConfig
	templates   map[string]subagent.Template
	policy      *permission.Policy

	// openCodeGo reads the OpenCode Go model list when its key was stored.
	openCodeGo *openCodeGoCatalog
	// subagentModel is the file of /model-sub's choice; empty without the
	// launcher or with an operator's templates.
	subagentModel string
}

func (h modelUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	defer clear(body)
	var peek struct{ Action string }
	if err != nil || json.Unmarshal(body, &peek) != nil {
		http.Error(w, "invalid local request", 400)
		return
	}
	if peek.Action != "launcher.check" && peek.Action != "runtime.health" && peek.Action != "model.catalog" && peek.Action != "model.select" && peek.Action != "model.providers" && peek.Action != "child.start" && peek.Action != "subagent.providers" && peek.Action != "subagent.select" {
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.next.ServeHTTP(w, r)
		return
	}
	var q modelRequest
	out := modelReply{}
	if json.Unmarshal(body, &q, json.RejectUnknownMembers(true)) != nil {
		out.Error = "invalid_request"
	} else if peek.Action == "launcher.check" {
		if q.HostBinding == nil || h.binding == nil || *q.HostBinding != *h.binding {
			out.Error = "host_configuration_mismatch"
		}
	} else if peek.Action == "runtime.health" {
		out.Capabilities = make(map[string]modelcatalog.Capabilities)
		for id, c := range h.runtime.Capabilities {
			if buildfeatures.Disabled(id) {
				continue
			}
			out.Capabilities[id] = c
		}
		ids := append([]string{h.runtime.Identity.Provider.Provider}, h.runtime.Discovered.IDs()...)
		for id := range h.runtime.Backends {
			ids = append(ids, id)
		}
		for _, id := range ids {
			if buildfeatures.Disabled(id) {
				continue
			}
			out.Capabilities[id] = modelcatalog.Capabilities{Tools: h.runtime.ProviderSupportsTools(id)}
		}
	} else {
		s, e := h.owner.Attach(q.ID)
		if e != nil {
			out.Error = "session_unavailable"
		} else {
			if peek.Action == "child.start" {
				templates := h.templates
				chosen, _ := readSubagentModel(h.subagentModel)
				if h.runtime.BuiltinSubagents && q.ChildRuntime != nil && q.ChildRuntime.Provider == "" && chosen.Provider != opencodego.ProviderID && chosen.Provider != claudeagent.ProviderID {
					templates = make(map[string]subagent.Template, len(h.templates))
					for name, t := range h.templates {
						templates[name] = t
					}
					if t, ok := templates[q.Template]; ok {
						var err error
						id := builtinGPTProvider(s)
						if chosen.Provider == "openai-codex" {
							id = chosen.Provider
						}
						templates[q.Template], err = builtinGPTTemplate(h.runtime, t, id)
						if err != nil {
							out.Error = "provider_unavailable"
							h.writeReply(w, out)
							return
						}
					}
				}
				resolve := childRuntimeResolver(h.runtime, h.subagentModel)
				if len(h.runtime.Backends) == 0 && h.runtime.Discovered == nil {
					resolve = nil
				}
				// The resolver bounds the start by the Session's own capability;
				// the gateway's read-only baseline would only narrow that, and the
				// child then follows the Session's mode itself.
				_, retry := s.OperationIntent(q.InputID)
				var receipt host.Receipt
				e := subagentOverrideRefusal(h.subagentModel, h.templates[q.Template], q.ChildRuntime, retry)
				if e == nil && !retry && q.ChildRuntime != nil && q.ChildRuntime.Provider == "" && claudeModel(q.ChildRuntime.Model) {
					selected := sessionstore.SelectionFromConfiguration(templates[q.Template].Runtime)
					if selected != nil && (selected.Provider == "openai-codex" || selected.Provider == "openai") {
						e = &subagent.RuntimeError{Code: "invalid_model", Hint: fmt.Sprintf("%q is a Claude model; subagent start runs its template's provider (ChatGPT). For a Claude model, use Claude Code's Agent tool with that model", q.ChildRuntime.Model)}
					}
				}
				if e == nil {
					receipt, e = subagent.StartChild(r.Context(), s, q.Generation, q.InputID, q.Template, q.Task, templates, q.ChildRuntime, resolve)
				}
				if e != nil {
					out.Error = "child_start_failed"
					var re *subagent.RuntimeError
					if errors.As(e, &re) {
						out.Error = re.Code
						out.Hint = re.Hint
					}
				} else {
					out.Receipt = &receipt
				}
				h.writeReply(w, out)
				return
			}
			if peek.Action == "subagent.providers" || peek.Action == "subagent.select" {
				h.subagentChoice(r.Context(), peek.Action == "subagent.select", q.Selection, &out)
				h.writeReply(w, out)
				return
			}
			if q.Refresh && h.rediscover != nil {
				if err := h.rediscover(r.Context()); err != nil {
					out.Error = "provider_check_failed"
					h.writeReply(w, out)
					return
				}
			}
			active, _ := s.RuntimeSelection()
			if peek.Action == "model.providers" {
				out.Providers = h.providers(r.Context())
				h.writeReply(w, out)
				return
			}
			id := q.Provider
			if id == "" && active != nil {
				id = active.Provider
			}
			if peek.Action == "model.select" {
				id = q.Selection.Provider
				if id == "" && active != nil {
					id = active.Provider
				}
			}
			if buildfeatures.Disabled(id) {
				out.Error, out.Hint = buildfeatures.OpenCodeGoDisabledCode, buildfeatures.OpenCodeGoDisabledMessage
				h.writeReply(w, out)
				return
			}
			if peek.Action == "model.catalog" && id == loginProvider {
				out.Catalog = &modelcatalog.Catalog{Problem: loginInstructions}
				h.writeReply(w, out)
				return
			}
			if q.Refresh && peek.Action == "model.catalog" && id == claudeagent.ProviderID {
				_, _ = h.catalog(r.Context(), id, true)
			}
			if len(h.supported) > 0 {
				available := false
				if peek.Action == "model.catalog" && q.Refresh {
					// Rediscovery has published authenticated backend registrations.
					_, registered := h.runtime.ProviderRuntime(id)
					available = registered == nil && h.runtime.CheckProvider(r.Context(), id) == nil
				} else {
					for _, p := range h.providers(r.Context()) {
						if p.ID == id && p.Availability == "available" {
							available = true
						}
					}
				}
				if !available {
					out.Error = "provider_unavailable"
					h.writeReply(w, out)
					return
				}
			}
			c, e := h.catalog(r.Context(), id, q.Refresh && peek.Action == "model.catalog")
			if e != nil {
				c = modelcatalog.Catalog{Problem: "catalog discovery unavailable"}
			}

			if peek.Action == "model.catalog" {
				out.Catalog = &c
			} else {
				m, e := c.Find(q.Selection.Model)
				if e != nil && !c.Authoritative && active != nil && id == active.Provider && active.Model == q.Selection.Model && active.Effort == q.Selection.Effort {
					m = modelcatalog.Model{ID: active.Model, Name: active.Name, Efforts: []llm.ReasoningEffort{active.Effort}, ContextWindow: active.ContextWindow}
					e = nil
				}
				if active == nil || e != nil || !m.AllowsEffort(q.Selection.Effort) {
					out.Error = "selection_unavailable"
				} else {
					choice := q.Selection
					choice.Provider, choice.Model, choice.Name, choice.ContextWindow = id, m.ID, viewer.SafeText(m.Name), m.ContextWindow
					if h.runtime.Providers != nil {
						var bindErr error
						choice, bindErr = h.runtime.BindSelection(choice)
						if bindErr != nil {
							out.Error = "provider_unavailable"
						}
					}
					if out.Error != "" {
						h.writeReply(w, out)
						return
					}
					if e = s.SelectRuntime(r.Context(), q.Generation, q.Expected, choice); e != nil {
						switch {
						case errors.Is(e, host.ErrConflict):
							out.Error = "selection_changed; reopen /model"
						case errors.Is(e, localfile.ErrWriterOwned):
							out.Error = "writer_owned"
						case errors.Is(e, host.ErrStopped):
							out.Error = "session_stopped"
						case errors.Is(e, host.ErrStaleGeneration):
							out.Error = "stale_generation"
						default:
							out.Error = "selection_failed"
						}
					}
				}
			}
		}
	}
	h.writeReply(w, out)
}

// subagentChoice lists or saves the sub-agents' model for /model-sub:
// Codex, the templates as built, an available OpenCode Go model, or a Claude
// model for Claude Code's own Agent sub-agents.
func (h modelUI) subagentChoice(ctx context.Context, save bool, selection sessionstore.RuntimeSelection, out *modelReply) {
	if h.subagentModel == "" {
		out.Error = "subagent_model_unavailable"
		return
	}
	providers := h.subagentProviders(ctx)
	if !save {
		m, err := readSubagentModel(h.subagentModel)
		if err != nil {
			out.Error = "subagent_model_unreadable"
			return
		}
		if m.Provider == "" {
			m.Provider = "openai-codex"
		}
		out.Providers, out.SubagentModel = providers, &sessionstore.RuntimeSelection{Version: 1, Provider: m.Provider, Model: m.Model}
		return
	}
	if buildfeatures.Disabled(selection.Provider) {
		out.Error, out.Hint = buildfeatures.OpenCodeGoDisabledCode, buildfeatures.OpenCodeGoChildDisabledMessage
		return
	}
	chosen := subagentModel{Version: 1, Provider: selection.Provider}
	name := ""
	switch selection.Provider {
	case "openai-codex":
	case opencodego.ProviderID, claudeagent.ProviderID:
		available := slices.ContainsFunc(providers, func(p modelcatalog.Provider) bool { return p.ID == selection.Provider && p.Availability == "available" })
		c, err := h.catalog(ctx, selection.Provider, false)
		m, findErr := c.Find(selection.Model)
		if !available || err != nil || findErr != nil {
			out.Error = "selection_unavailable"
			return
		}
		chosen.Model, name = m.ID, viewer.SafeText(m.Name)
	default:
		out.Error = "selection_unavailable"
		return
	}
	if err := writeSubagentModel(h.subagentModel, chosen); err != nil {
		out.Error = "subagent_model_unwritable"
		return
	}
	out.SubagentModel = &sessionstore.RuntimeSelection{Version: 1, Provider: chosen.Provider, Model: chosen.Model, Name: name}
	out.Pending = len(h.templates) == 0
}

// subagentProviders are the providers /model-sub offers, OpenCode Go also
// before its key is saved, then Claude when /model lists it.
func (h modelUI) subagentProviders(ctx context.Context) []modelcatalog.Provider {
	all := h.providers(ctx)
	var out []modelcatalog.Provider
	for _, id := range []string{"openai-codex", opencodego.ProviderID} {
		if buildfeatures.Disabled(id) {
			continue
		}
		if i := slices.IndexFunc(all, func(p modelcatalog.Provider) bool { return p.ID == id }); i >= 0 {
			out = append(out, all[i])
		} else {
			out = append(out, modelcatalog.Provider{ID: id, Name: modelcatalog.ProviderName(id), Availability: "unavailable", Reason: modelcatalog.AuthRequired})
		}
	}
	// The Claude choice names Claude Code's Agent model, so it is offered only
	// where /model lists Claude at all.
	if i := slices.IndexFunc(all, func(p modelcatalog.Provider) bool { return p.ID == claudeagent.ProviderID }); i >= 0 {
		out = append(out, all[i])
	}
	return out
}

func (h modelUI) writeReply(w http.ResponseWriter, out modelReply) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	encoded, e := json.Marshal(out)
	if e != nil {
		encoded = []byte(`{"Error":"invalid_reply"}`)
	}
	w.Write(encoded)
}
func modelExchange(ctx context.Context, call viewer.ExtensionCall, q modelRequest) (modelReply, error) {
	b, e := json.Marshal(q)
	if e != nil {
		return modelReply{}, e
	}
	raw, e := call(ctx, b)
	if e != nil {
		return modelReply{}, e
	}
	var out modelReply
	if json.Unmarshal(raw, &out, json.RejectUnknownMembers(true)) != nil {
		return out, errors.New("invalid model selection reply")
	}
	if out.Error != "" {
		if out.Hint != "" {
			return out, errors.New(out.Error + ": " + out.Hint)
		}
		return out, errors.New(out.Error)
	}
	return out, nil
}
func codexModelCache(auth string) string {
	if auth == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(auth), "models_cache.json")
}
