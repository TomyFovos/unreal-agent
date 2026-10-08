package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/privateexport"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

// ExportError deliberately contains only a closed code, never a filesystem,
// gateway, provider, or message body. Export notifications are TUI-local.
type ExportError struct{ Code string }

func (e *ExportError) Error() string {
	switch e.Code {
	case "no_responses":
		return "no public agent responses to export"
	case "no_conversation":
		return "no public conversation to export"
	case "response_unavailable":
		return "selected public response unavailable; reopen /export"
	case "response_not_exportable":
		return "latest public agent response is not exportable; use /export to choose another response"
	case "history_unavailable":
		return "canonical history unavailable or incomplete; reconnect and retry"
	case "canceled":
		return "export canceled"
	default:
		return "private Markdown export unavailable (requires an owned 0700 export directory)"
	}
}

// ResponseChoice holds a canonical source reference and a short safe preview,
// not the rendered transcript or a cached copy of the full response body.
type ResponseChoice struct {
	Sequence   sessionstore.Sequence
	TurnNumber int
	Preview    string
}

type exportRuntime struct{ provider, model, effort string }
type exportTurn struct {
	sequence sessionstore.Sequence
	number   int
	at       time.Time
	runtime  exportRuntime
	internal bool
}
type conversationIndex struct {
	turns     map[session.TurnID]exportTurn
	order     []exportTurn
	responses []ResponseChoice
	users     int
	latest    sessionstore.Sequence
}

// Capture the canonical tail from the Host, including messages not yet seen by
// the live subscription. This local read cursor never replaces its cursor or
// changes the TUI window. Once captured, all export passes use this fixed prefix.
func exportBoundary(ctx context.Context, r Reader, id session.ID, after sessionstore.Sequence) (sessionstore.Sequence, error) {
	for {
		if ctx.Err() != nil {
			return 0, &ExportError{Code: "canceled"}
		}
		v, err := r.Inspect(ctx, id, after, 128)
		if err != nil {
			return 0, exportFailure(ctx, err, "history_unavailable")
		}
		progress := false
		for _, item := range v.History.Items {
			if item.Sequence <= after {
				continue
			}
			if item.Sequence != after+1 {
				return 0, &ExportError{Code: "history_unavailable"}
			}
			after, progress = item.Sequence, true
		}
		if !v.History.More {
			return after, nil
		}
		if !progress {
			return 0, &ExportError{Code: "history_unavailable"}
		}
	}
}

// Public eligibility is an explicit allowlist. Credential entry is not an
// external Input; controls, peers, tools, reasoning, replay IDs, account data,
// and arbitrary provider output shapes never become export bodies.
func exportText(raw string) string {
	text := SafeText(raw)
	if strings.TrimSpace(text) == "" || secretguard.Sensitive(raw) || text != raw && secretguard.Sensitive(text) {
		return ""
	}
	return text
}

func publicAgentPart(output llm.Item) (llm.Message, bool) {
	m, ok := output.Data.(llm.Message)
	// Empty Role is the legacy public assistant shape already accepted by
	// the transcript projection; explicit user/system/tool roles are not.
	if !ok || output.Type != llm.ItemMessage || m.Role != llm.RoleAssistant && m.Role != "" || m.Text == "" {
		return llm.Message{}, false
	}
	return m, m.Phase == "" || m.Phase == "commentary" || m.Phase == "final" || m.Phase == "final_answer"
}

func publicAgentText(response sessionstore.ModelResponse) string {
	var parts []string
	for _, output := range response.Response.Output {
		m, ok := publicAgentPart(output)
		if !ok {
			continue
		}
		// A protected public part excludes the entire response, so a listed
		// choice always represents its complete eligible public response.
		text := SafeText(m.Text)
		if secretguard.Sensitive(m.Text) || text != m.Text && secretguard.Sensitive(text) {
			return ""
		}
		parts = append(parts, text)
	}
	text := strings.Join(parts, "\n\n")
	if strings.TrimSpace(text) == "" || len(parts) > 1 && secretguard.Sensitive(text) {
		return ""
	}
	return text
}

func publicUserText(input inbox.Input) string {
	if input.Kind != inbox.InputExternal {
		return ""
	}
	var text string
	if json.Unmarshal(input.Payload, &text) != nil {
		return ""
	}
	return exportText(text)
}

func indexConversation(ctx context.Context, r Reader, id session.ID, through sessionstore.Sequence) (conversationIndex, error) {
	index := conversationIndex{turns: make(map[session.TurnID]exportTurn)}
	var active *sessionstore.RuntimeSelection
	err := readHistory(ctx, r, id, 0, through, func(item host.HistoryItem) bool {
		switch item.Kind {
		case sessionstore.ItemHostRecord:
			if record, ok := item.Data.(sessionstore.HostRecord); ok {
				switch record.Kind {
				case "configuration":
					if active == nil {
						active = sessionstore.SelectionFromConfiguration(record.Configuration)
					}
				case sessionstore.HostRuntimeApplied:
					active = nil
					if record.Selection != nil && record.Selection.Validate() == nil {
						choice := *record.Selection
						active = &choice
					}
				}
			}
		case sessionstore.ItemFork:
			active = nil
		case sessionstore.ItemInput:
			if input, ok := item.Data.(inbox.Input); ok {
				if publicUserText(input) != "" {
					index.users++
				}
				if control, err := input.DecodeControlMessage(); err == nil && control.Mode == inbox.UpdateSettings && active != nil {
					choice := *active
					choice.Effort = control.Parameters.(inbox.Settings).ReasoningEffort
					active = &choice
				}
			}
		case sessionstore.ItemTurn:
			if turn, ok := item.Data.(session.Turn); ok {
				t := exportTurn{sequence: item.Sequence, number: len(index.order) + 1, at: item.RecordedAt, internal: turn.Type == session.TurnCompaction}
				if active != nil && active.Revision == turn.RuntimeRevision {
					t.runtime = exportRuntime{active.Provider, active.Model, string(active.Effort)}
				}
				index.turns[turn.ID] = t
				index.order = append(index.order, t)
			}
		case sessionstore.ItemModelResponse:
			if response, ok := item.Data.(sessionstore.ModelResponse); ok {
				turn := index.turns[response.TurnID]
				if turn.internal {
					break
				}
				for _, output := range response.Response.Output {
					if _, ok := publicAgentPart(output); ok {
						index.latest = item.Sequence
						break
					}
				}
				if text := publicAgentText(response); text != "" {
					preview := strings.Join(strings.Fields(clipBytes(text, 512)), " ")
					if len(preview) > 240 {
						preview = clipBytes(preview, 240) + "…"
					} else if len(text) > 512 {
						preview += "…"
					}
					index.responses = append(index.responses, ResponseChoice{item.Sequence, turn.number, strings.Clone(preview)})
				}
			}
		}
		return true
	})
	if err != nil {
		return conversationIndex{}, exportFailure(ctx, err, "history_unavailable")
	}
	return index, nil
}

// ResponseChoices scans the whole canonical prefix, independently of the live
// display window. Newest first; no body of a private/non-exportable response is
// retained in picker state.
func ResponseChoices(ctx context.Context, r Reader, id session.ID, through sessionstore.Sequence) ([]ResponseChoice, error) {
	index, err := indexConversation(ctx, r, id, through)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(index.responses)-1; i < j; i, j = i+1, j-1 {
		index.responses[i], index.responses[j] = index.responses[j], index.responses[i]
	}
	return index.responses, nil
}

func ExportLastResponse(ctx context.Context, r Reader, id session.ID, through sessionstore.Sequence, dir string) (string, error) {
	index, err := indexConversation(ctx, r, id, through)
	if err != nil {
		return "", err
	}
	if index.latest == 0 {
		return "", &ExportError{Code: "no_responses"}
	}
	if len(index.responses) == 0 || index.responses[len(index.responses)-1].Sequence != index.latest {
		return "", &ExportError{Code: "response_not_exportable"}
	}
	return ExportResponse(ctx, r, id, index.responses[len(index.responses)-1], dir)
}

// ExportResponse rereads the selected canonical record, not its preview. One
// response may have multiple public message parts, joined by two newlines.
func ExportResponse(ctx context.Context, r Reader, id session.ID, choice ResponseChoice, dir string) (string, error) {
	if choice.Sequence == 0 {
		return "", &ExportError{Code: "response_unavailable"}
	}
	var text string
	internalTurns := make(map[session.TurnID]bool)
	err := readHistory(ctx, r, id, 0, choice.Sequence, func(item host.HistoryItem) bool {
		if turn, ok := item.Data.(session.Turn); ok && item.Kind == sessionstore.ItemTurn {
			internalTurns[turn.ID] = turn.Type == session.TurnCompaction
		}
		if response, ok := item.Data.(sessionstore.ModelResponse); ok && item.Kind == sessionstore.ItemModelResponse && item.Sequence == choice.Sequence && !internalTurns[response.TurnID] {
			text = publicAgentText(response)
		}
		return true
	})
	if err != nil {
		return "", exportFailure(ctx, err, "history_unavailable")
	}
	if text == "" {
		return "", &ExportError{Code: "response_unavailable"}
	}
	return writeMarkdown(ctx, dir, "response", func(w io.Writer) error {
		_, err := io.WriteString(w, text)
		return err
	})
}

// ExportConversation makes two paged passes over an immutable canonical prefix:
// small turn metadata first, then bodies written incrementally. It never builds
// a session-sized string or reads the Context Engine's selected context.
func ExportConversation(ctx context.Context, r Reader, id session.ID, through sessionstore.Sequence, dir string) (string, error) {
	index, err := indexConversation(ctx, r, id, through)
	if err != nil {
		return "", err
	}
	if index.users == 0 && len(index.responses) == 0 {
		return "", &ExportError{Code: "no_conversation"}
	}
	return writeMarkdown(ctx, dir, "session", func(w io.Writer) error {
		if _, err := fmt.Fprintf(w, "# Unreal Agent Session Export\n\nSession: %s\nExported: %s\n", metadataText(string(id)), time.Now().Format("2006-01-02 15:04:05 MST")); err != nil {
			return err
		}
		var writeErr error
		lastSection := -1
		err := readHistory(ctx, r, id, 0, through, func(item host.HistoryItem) bool {
			var role, text, observed string
			var turn exportTurn
			switch item.Kind {
			case sessionstore.ItemInput:
				if input, ok := item.Data.(inbox.Input); ok {
					text, role = publicUserText(input), "You"
					// Canonical external inputs precede the Turn that consumes
					// them. Unconsumed input stays in an unnumbered section.
					i := sort.Search(len(index.order), func(i int) bool { return index.order[i].sequence > item.Sequence })
					if i < len(index.order) {
						turn = index.order[i]
					}
				}
			case sessionstore.ItemModelResponse:
				if response, ok := item.Data.(sessionstore.ModelResponse); ok {
					turn = index.turns[response.TurnID]
					if !turn.internal {
						text, role, observed = publicAgentText(response), "Agent", metadataText(response.Response.Model)
					}
				}
			}
			if text == "" {
				return true
			}
			if lastSection != turn.number {
				if turn.number == 0 {
					_, writeErr = io.WriteString(w, "\n## Conversation (turn unknown)\n")
				} else {
					_, writeErr = fmt.Fprintf(w, "\n## Turn %d\n", turn.number)
				}
				if writeErr == nil {
					writeErr = writeTurnMetadata(w, turn)
				}
				lastSection = turn.number
			}
			if writeErr == nil {
				_, writeErr = fmt.Fprintf(w, "\n### %s\n\n", role)
			}
			if writeErr == nil && observed != "" {
				_, writeErr = fmt.Fprintf(w, "Observed model: %s\n\n", observed)
			}
			if writeErr == nil {
				_, writeErr = io.WriteString(w, text)
			}
			if writeErr == nil {
				_, writeErr = io.WriteString(w, "\n")
			}
			return writeErr == nil
		})
		if err != nil {
			return exportFailure(ctx, err, "history_unavailable")
		}
		return writeErr
	})
}

func metadataText(raw string) string {
	text := exportText(raw)
	if len(text) > 256 || strings.ContainsAny(text, "\n\t") {
		return ""
	}
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]").Replace(text)
}

func writeTurnMetadata(w io.Writer, turn exportTurn) error {
	if !turn.at.IsZero() {
		if _, err := fmt.Fprintf(w, "\nTimestamp: %s\n", turn.at.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{{"Provider", turn.runtime.provider}, {"Model", turn.runtime.model}, {"Effort", turn.runtime.effort}} {
		if value := metadataText(pair[1]); value != "" {
			if _, err := fmt.Fprintf(w, "%s: %s\n", pair[0], value); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeMarkdown(ctx context.Context, dir, prefix string, write func(io.Writer) error) (string, error) {
	if dir == "" {
		root := os.Getenv("XDG_STATE_HOME")
		if root == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", &ExportError{Code: "file_unavailable"}
			}
			root = filepath.Join(home, ".local", "state")
		}
		dir = filepath.Join(root, "unreal-agent", "exports")
	}
	path, err := privateexport.WriteStream(ctx, dir, prefix, "md", write)
	if err != nil {
		return "", exportFailure(ctx, err, "file_unavailable")
	}
	return path, nil
}

func exportFailure(ctx context.Context, err error, code string) error {
	var safe *ExportError
	if errors.As(err, &safe) {
		return safe
	}
	if ctx.Err() != nil {
		code = "canceled"
	}
	return &ExportError{Code: code}
}
