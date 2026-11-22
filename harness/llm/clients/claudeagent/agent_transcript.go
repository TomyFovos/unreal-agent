package claudeagent

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool/observed"
)

// agentTranscripts holds the public blocks reported within each Agent call.
// Claude Code owns their conversation. Only the outer Agent call and result
// are part of Unreal's model history. Access is serialized by turn.mu.
type agentTranscripts struct {
	agents   map[string]*agentTranscript
	parents  map[string]string
	awaiting map[string]observed.Result // a hook can precede the public tool_use
}

type agentTranscript struct {
	blocks  []llm.ToolObservation
	seen    map[agentText]bool
	tools   map[string]string // tool ID to a potential written path
	results map[string]int
}

type agentText struct {
	message string
	kind    llm.ItemType
	text    string
}

func (a *agentTranscripts) transcript(parent string) *agentTranscript {
	if a.agents == nil {
		a.agents = make(map[string]*agentTranscript)
		a.parents = make(map[string]string)
	}
	if a.agents[parent] == nil {
		a.agents[parent] = &agentTranscript{seen: make(map[agentText]bool), tools: make(map[string]string), results: make(map[string]int)}
	}
	return a.agents[parent]
}

func (a *agentTranscripts) assistant(parent string, m message) {
	transcript := a.transcript(parent)
	for _, b := range m.Content {
		entry := llm.ToolObservation{ParentCallID: parent, Model: m.Model}
		switch b.Type {
		case "text":
			entry.Type, entry.Text = llm.ItemMessage, b.Text
		case "thinking":
			entry.Type, entry.Text = llm.ItemReasoning, first(b.Thinking, b.Text)
		case "redacted_thinking":
			entry.Type = llm.ItemReasoning
		case "tool_use":
			if b.ID == "" {
				continue
			}
			if _, seen := transcript.tools[b.ID]; seen {
				continue
			}
			entry.Type, entry.CallID, entry.Tool = llm.ItemToolCall, b.ID, b.Name
			transcript.tools[b.ID] = writtenPath(b.Name, b.Input)
			a.parents[b.ID] = parent
		default:
			continue
		}
		if entry.CallID == "" && m.ID != "" {
			key := agentText{m.ID, entry.Type, entry.Text}
			if transcript.seen[key] {
				continue
			}
			transcript.seen[key] = true
		}
		transcript.blocks = append(transcript.blocks, entry)
		if result, ok := a.awaiting[entry.CallID]; ok {
			a.result(parent, entry.CallID, result, false)
			delete(a.awaiting, entry.CallID)
		}
	}
}

func (a *agentTranscripts) result(parent, id string, result observed.Result, replace bool) {
	if parent == "" {
		if a.awaiting == nil {
			a.awaiting = make(map[string]observed.Result)
		}
		a.awaiting[id] = result
		return
	}
	transcript := a.transcript(parent)
	a.parents[id] = parent
	delete(a.awaiting, id)
	entry := llm.ToolObservation{ParentCallID: parent, Type: llm.ItemToolResult, CallID: id, Text: result.Content, IsError: result.IsError}
	if index, ok := transcript.results[id]; ok {
		if replace {
			transcript.blocks[index] = entry
		}
		return
	}
	transcript.results[id] = len(transcript.blocks)
	transcript.blocks = append(transcript.blocks, entry)
}

// take returns the Agent transcript and propagates successful workspace writes
// to its canonical receipt, including writes from tools inside a nested Agent.
func (a *agentTranscripts) take(parent string) ([]llm.ToolObservation, []string) {
	transcript := a.agents[parent]
	if transcript == nil {
		return nil, nil
	}
	delete(a.agents, parent)
	var entries []llm.ToolObservation
	var paths []string
	for _, entry := range transcript.blocks {
		entries = append(entries, entry)
		switch entry.Type {
		case llm.ItemToolCall:
			inner, written := a.take(entry.CallID)
			entries = append(entries, inner...)
			paths = append(paths, written...)
		case llm.ItemToolResult:
			if path := transcript.tools[entry.CallID]; !entry.IsError && path != "" {
				paths = append(paths, path)
			}
		}
	}
	return entries, paths
}
