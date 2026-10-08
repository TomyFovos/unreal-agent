package tui

import (
	"encoding/json/v2"
	"strings"
	"time"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/cmd/internal/tui/terminaltext"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

const clippedMessage = "[clipped for display; canonical history retained]"
const olderMessage = "earlier history available; PgUp loads the previous page"

const messageDisplayBytes = 64 << 10
const transcriptDisplayBytes = 2 << 20
const transcriptDisplayEntries = 1024

// Entry is bounded frontend data, derived only from canonical history. It is
// never written back to the Host, Session, or delegated task.
type Entry struct {
	Role, Text, Code string
	PeerID, PeerKind string
	TurnID           session.TurnID
	At               time.Time
	Sequence         sessionstore.Sequence
	Clipped          bool
	// Sanitization must not turn a rejected provider URL into a clickable one.
	UnsafeLinks bool
	Calls       []Receipt
}

func cloneEntries(in []Entry) []Entry {
	out := append([]Entry(nil), in...)
	for i := range out {
		out[i].Calls = append([]Receipt(nil), out[i].Calls...)
		for j := range out[i].Calls {
			out[i].Calls[j].Operations = append([]operation.ID(nil), out[i].Calls[j].Operations...)
		}
	}
	return out
}

func clipBytes(s string, bound int) string {
	s = SafeText(s)
	if len(s) <= bound {
		return s
	}
	g := uniseg.NewGraphemes(s)
	end := 0
	for g.Next() {
		_, next := g.Positions()
		if next > bound {
			break
		}
		end = next
	}
	return s[:end]
}

func entriesFor(item host.HistoryItem) []Entry {
	var result []Entry
	add := func(role, text, code string) {
		safe := terminaltext.Clean(text)
		result = append(result, Entry{Role: role, Text: clipBytes(safe, messageDisplayBytes), Code: code, At: item.RecordedAt, Sequence: item.Sequence, Clipped: len(safe) > messageDisplayBytes, UnsafeLinks: safe != text})
	}
	switch d := item.Data.(type) {
	case inbox.Input:
		switch d.Kind {
		case inbox.InputExternal:
			var value string
			if json.Unmarshal(d.Payload, &value) == nil {
				add("you", value, "")
			}
		case inbox.InputPeer:
			if peer, err := d.DecodePeerMessage(); err == nil {
				add("peer", peer.Text, "")
				result[0].PeerID, result[0].PeerKind = SafeText(peer.Sender), SafeText(peer.Kind)
			}
		case inbox.InputControl:
			var control inbox.ControlMessage
			if json.Unmarshal(d.Payload, &control) == nil {
				add("host", "control: "+string(control.Mode), "")
			}
		case inbox.InputCrash:
			add("host", "crash recorded", "")
		}
	case sessionstore.ModelResponse:
		var calls []Receipt
		for _, output := range d.Response.Output {
			if msg, ok := output.Data.(llm.Message); ok {
				if output.Type != llm.ItemMessage || msg.Role != "" && msg.Role != llm.RoleAssistant || msg.Phase != "" && msg.Phase != "commentary" && msg.Phase != "final" && msg.Phase != "final_answer" {
					continue
				}
				add("agent", msg.Text, "")
				result[len(result)-1].TurnID = d.TurnID
			}
			if call, ok := output.Data.(llm.ToolCall); ok {
				calls = append(calls, newReceipt(call))
			}
		}
		if len(calls) > 0 {
			if len(result) == 0 {
				add("agent", "", "")
				result[0].TurnID = d.TurnID
			}
			result[len(result)-1].Calls = calls
		}
		if f := d.Response.Failure; f != nil {
			add("error", f.Message, f.Code)
		}
	case sessionstore.HostRecord:
		add("host", "host: "+strings.Join(strings.Fields(d.Kind), " "), "")
	}
	return result
}

// The terminal retains a bounded page; older canonical data is loaded through
// Reader, independently of Context Engine selection or the live model cursor.
func boundEntries(entries []Entry, limit int) ([]Entry, bool) {
	bytes, start := 0, len(entries)
	for start > 0 && len(entries)-start < limit {
		n := len(entries[start-1].Text) + len(entries[start-1].Calls)*512 + 128
		if bytes+n > transcriptDisplayBytes {
			break
		}
		bytes += n
		start--
	}
	if start == 0 {
		return entries, false
	}
	return append([]Entry(nil), entries[start:]...), true
}
