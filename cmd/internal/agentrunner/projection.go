package agentrunner

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/ast"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
)

// runnerProjection is a disposable attachment. Canonical history, TUI and the
// headless Host retain their existing representation and sequence numbers.
type runnerProjection struct {
	output               io.Writer
	log                  io.Closer
	directory, workspace string
	parent               session.ID
	parentAfter          sessionstore.Sequence
	sequence             sessionstore.Sequence
	children             []session.ID
	seenChildren         map[session.ID]bool
	started, ended       map[string]bool
	childEvents          map[string]map[string]any
	changed              map[string]bool
	usage                llm.Usage
	tools                int
	parentFailure        bool
}

type runnerItem struct {
	Sequence       sessionstore.Sequence
	RecordedAt     time.Time
	Kind           string
	Data           any
	Synthetic      bool                  `json:",omitzero"`
	ChildID        session.ID            `json:",omitzero"`
	SourceSequence sessionstore.Sequence `json:",omitzero"`
}

func newRunnerProjection(output io.Writer) *runnerProjection {
	return &runnerProjection{output: output, seenChildren: map[session.ID]bool{}, started: map[string]bool{}, ended: map[string]bool{}, childEvents: map[string]map[string]any{}, changed: map[string]bool{}}
}

func (p *runnerProjection) Write(raw []byte) (int, error) {
	var header struct{ Kind string }
	if json.Unmarshal(raw, &header) != nil || header.Kind == "" {
		return p.output.Write(raw) // CLI version, or the existing type:error item.
	}
	var item host.HistoryItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return 0, err
	}
	if err := p.observe(p.parent, item, false); err != nil {
		return 0, err
	}
	return len(raw), nil
}

func (p *runnerProjection) emit(kind string, data any, child session.ID) error {
	return p.write(runnerItem{Kind: kind, Data: data, RecordedAt: time.Now().UTC(), Synthetic: true, ChildID: child})
}

func (p *runnerProjection) write(item runnerItem) error {
	p.sequence++
	item.Sequence = p.sequence
	encoded, err := json.Marshal(item)
	if err != nil {
		return err
	}
	n, err := p.output.Write(append(encoded, '\n'))
	if err == nil && n != len(encoded)+1 {
		err = io.ErrShortWrite
	}
	return err
}

func (p *runnerProjection) observe(id session.ID, item host.HistoryItem, child bool) error {
	childID := session.ID("")
	if child {
		childID = id
	}
	switch data := item.Data.(type) {
	case sessionstore.ModelResponse:
		if child {
			// Preserve usage (including Raw) and calls, while keeping child final
			// messages from becoming the measured parent's final answer.
			data.Response.Output = slices.DeleteFunc(slices.Clone(data.Response.Output), func(entry llm.Item) bool { return entry.Type != llm.ItemToolCall })
			item.Data = data
		} else {
			p.parentFailure = data.Response.Failure != nil || data.Response.Stop == llm.StopRefused || data.Response.Stop == llm.StopMaxOutputTokens
		}
		p.addUsage(data.Response.Usage)
		for _, entry := range data.Response.Output {
			if entry.Type == llm.ItemToolCall {
				p.tools++
			}
		}
	case sessionstore.HostRecord:
		if data.Context != nil {
			if data.Context.Usage != nil {
				p.addUsage(*data.Context.Usage)
			}
			if data.Context.Conversation != nil && data.Context.Conversation.Usage != nil {
				p.addUsage(*data.Context.Conversation.Usage)
			}
		}
	}
	if err := p.write(runnerItem{Kind: string(item.Kind), Data: item.Data, RecordedAt: item.RecordedAt, Synthetic: child, ChildID: childID, SourceSequence: item.Sequence}); err != nil {
		return err
	}
	if !child {
		p.parentAfter = item.Sequence
	}
	if status, ok := item.Data.(sessionstore.ToolCallStatus); ok {
		for _, op := range status.Operations {
			state, err := operation.DecodeRemoteJobState(op)
			if err != nil {
				continue
			}
			switch state.Plan.Type {
			case subagent.PlanType:
				plan, err := subagent.DecodePlan(op)
				if err != nil || plan.Action != "start" || plan.ParentID != id {
					continue
				}
				if !p.seenChildren[plan.ChildID] {
					p.seenChildren[plan.ChildID] = true
					p.children = append(p.children, plan.ChildID)
				}
				key := string(id) + "/" + string(op.ID)
				event := map[string]any{"CallID": status.CallID, "Template": plan.Template, "Role": plan.Template, "ChildID": plan.ChildID}
				if !p.started[key] {
					event["Task"] = plan.Text
					if err := p.emit("subagent_start", event, childID); err != nil {
						return err
					}
					delete(event, "Task")
					p.started[key] = true
					p.childEvents[key] = event
				}
				if (op.Status == operation.StatusCompleted || op.Status == operation.StatusFailed || op.Status == operation.StatusCanceled) && !p.ended[key] {
					event["Status"] = op.Status
					if handle, err := subagent.DecodeHandle(op); err == nil && handle.Finish != nil {
						event["Status"], event["Summary"] = handle.Finish.Result.Status, handle.Finish.Result.Summary
					}
					if err := p.emit("subagent_end", event, childID); err != nil {
						return err
					}
					p.ended[key] = true
				}
			case native.PlanType, ast.PlanType:
				// Receipts, rather than proposed arguments, establish changes.
				// Failed multi-file edits can still have applied targets.
				var result struct {
					Mutation *mutation.Result `json:"mutation"`
				}
				if json.Unmarshal(state.Handle, &result) != nil || result.Mutation == nil {
					continue
				}
				name, change := "ast_edit", "modify"
				if state.Plan.Type == native.PlanType {
					var request native.Request
					if json.Unmarshal(state.Plan.Data, &request) != nil {
						continue
					}
					name = request.Action
					if request.Action == "write" && request.Expected != nil && !request.Expected.Exists {
						change = "add"
					}
				}
				for _, target := range result.Mutation.Targets {
					if target.Code != mutation.Applied {
						continue
					}
					path := target.Path
					if !filepath.IsAbs(path) {
						path = filepath.Join(p.workspace, path)
					}
					path = filepath.Clean(path)
					if resolved, err := filepath.EvalSymlinks(path); err == nil {
						path = resolved
					}
					if relative, err := filepath.Rel(p.workspace, path); err == nil {
						path = relative
					}
					if p.changed[path] {
						continue
					}
					if err := p.emit("file_change", map[string]any{"Path": path, "Change": change, "Tool": name, "CallID": status.CallID}, childID); err != nil {
						return err
					}
					p.changed[path] = true
				}
			}
		}
	}
	return nil
}

// Cancellation detaches the live subscriber before the Host drains. Read its
// final committed tail after Run has closed the owner so terminal receipts are
// still represented in the failed run's output.
func (p *runnerProjection) drainParent(ctx context.Context) error {
	if p.directory == "" || p.parent == "" {
		return nil
	}
	store, err := localfile.New(p.directory)
	if err != nil {
		return err
	}
	for {
		page, err := store.Items(ctx, p.parent, p.parentAfter, 256)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			if err := p.observe(p.parent, host.ProjectHistoryItem(item), false); err != nil {
				return err
			}
		}
		if !page.More {
			return nil
		}
	}
}

func (p *runnerProjection) endRemainingChildren(ctx context.Context, canceled bool) error {
	keys := make([]string, 0, len(p.childEvents))
	for key := range p.childEvents {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if p.ended[key] {
			continue
		}
		data := p.childEvents[key]
		data["Status"] = "failed"
		if canceled {
			data["Status"] = "canceled"
		}
		// A child may have finished immediately before the parent's detach.
		if id, ok := data["ChildID"].(session.ID); ok {
			if snapshot, err := subagent.ReadChildSnapshot(ctx, p.directory, id); err == nil && snapshot.Finish != nil {
				data["Status"], data["Summary"] = snapshot.Finish.Result.Status, snapshot.Finish.Result.Summary
			}
		}
		if err := p.emit("subagent_end", data, ""); err != nil {
			return err
		}
		p.ended[key] = true
	}
	return nil
}

func (p *runnerProjection) addUsage(u llm.Usage) {
	p.usage.InputTokens += u.InputTokens
	p.usage.OutputTokens += u.OutputTokens
	p.usage.CachedInputTokens += u.CachedInputTokens
	p.usage.CacheWriteInputTokens += u.CacheWriteInputTokens
	p.usage.ReasoningTokens += u.ReasoningTokens
	for _, field := range u.Unknown {
		if !slices.Contains(p.usage.Unknown, field) {
			p.usage.Unknown = append(p.usage.Unknown, field)
		}
	}
}

// Aggregate after the owning Host closes and drains children, also on failure.
// Pages and a visited set include grandchildren once without a history cap.
func (p *runnerProjection) aggregateChildren(ctx context.Context) error {
	if len(p.children) == 0 {
		return nil
	}
	store, err := localfile.New(p.directory)
	if err != nil {
		return err
	}
	for i := 0; i < len(p.children); i++ {
		id := p.children[i]
		for after := sessionstore.BeforeFirst; ; {
			page, err := store.Items(ctx, id, after, 256)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				return fmt.Errorf("read child history: %w", err)
			}
			for _, item := range page.Items {
				if item.Kind != sessionstore.ItemModelResponse && item.Kind != sessionstore.ItemToolCallStatus && item.Kind != sessionstore.ItemHostRecord {
					continue
				}
				if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind != sessionstore.HostContext {
					continue
				}
				if err := p.observe(id, host.ProjectHistoryItem(item), true); err != nil {
					return err
				}
			}
			if !page.More {
				break
			}
			after = page.NextAfter
		}
	}
	return nil
}

func (p *runnerProjection) finish(failed bool) error {
	paths := make([]string, 0, len(p.changed))
	for path := range p.changed {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	if failed || p.parentFailure {
		_, err := io.WriteString(p.output, "{\"type\":\"session_end\",\"is_error\":true}\n")
		return err
	}
	return p.emit("session_end", map[string]any{"IsError": false, "Usage": p.usage, "ToolCalls": p.tools, "ChangedFiles": paths}, "")
}
