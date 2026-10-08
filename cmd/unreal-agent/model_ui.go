package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
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
	Receipt      *host.Receipt                        `json:",omitzero"`
	Capabilities map[string]modelcatalog.Capabilities `json:",omitzero"`
}
type modelUI struct {
	binding    *launcherHostBinding
	owner      *host.Host
	cache      string
	claude     modelcatalog.Catalog
	claudeRead func(context.Context, bool) (modelcatalog.Catalog, error)
	next       http.Handler
	runtime    agentrunner.RuntimeConfig
	templates  map[string]subagent.Template
	policy     *permission.Policy
}

func (h modelUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	defer clear(body)
	var peek struct{ Action string }
	if err != nil || json.Unmarshal(body, &peek) != nil {
		http.Error(w, "invalid local request", 400)
		return
	}
	if peek.Action != "launcher.check" && peek.Action != "runtime.health" && peek.Action != "model.catalog" && peek.Action != "model.select" && peek.Action != "model.providers" && peek.Action != "child.start" {
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.next.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost || r.Host != "localhost" || r.Header.Get("Origin") != "" {
		http.Error(w, "invalid local request", 400)
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
		out.Capabilities = h.runtime.Capabilities
	} else {
		s, e := h.owner.Attach(q.ID)
		if e != nil {
			out.Error = "session_unavailable"
		} else {
			if peek.Action == "child.start" {
				resolve := childRuntimeResolver(h.runtime)
				if len(h.runtime.Backends) == 0 {
					resolve = nil
				}
				receipt, e := subagent.StartChild(permission.WithPolicy(r.Context(), h.policy), s, q.Generation, q.InputID, q.Template, q.Task, h.templates, q.ChildRuntime, resolve)
				if e != nil {
					out.Error = "child_start_failed"
					var re *subagent.RuntimeError
					if errors.As(e, &re) {
						out.Error = re.Code
					}
				} else {
					out.Receipt = &receipt
				}
				h.writeReply(w, out)
				return
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
