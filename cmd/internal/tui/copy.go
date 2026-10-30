package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// MessageChoice retains only a canonical reference and a safe first line.
// Part distinguishes separate public statements in one model response.
type MessageChoice struct {
	Sequence      sessionstore.Sequence
	Part          int
	Role, Preview string
}

func walkCopyMessages(ctx context.Context, r Reader, id session.ID, through sessionstore.Sequence, visit func(MessageChoice, string)) error {
	internal := map[session.TurnID]bool{}
	background := false
	return readHistory(ctx, r, id, 0, through, func(item host.HistoryItem) bool {
		emit := func(part int, role, text string) {
			if text == "" {
				return
			}
			first, _, _ := strings.Cut(text, "\n")
			visit(MessageChoice{Sequence: item.Sequence, Part: part, Role: role, Preview: strings.Clone(clipBytes(first, 240))}, text)
		}
		switch data := item.Data.(type) {
		case session.Turn:
			internal[data.ID] = data.Type == session.TurnCompaction
		case inbox.Input:
			if data.Kind == inbox.InputExternal {
				background = false
				emit(0, "You", publicUserText(data))
			}
			if data.Kind == inbox.InputPeer {
				if peer, err := data.DecodePeerMessage(); err != nil || !quietPeerKind(peer.Kind) {
					background = true
				}
			}
		case sessionstore.ModelResponse:
			if internal[data.TurnID] || publicAgentText(data) == "" {
				break
			}
			for part, output := range data.Response.Output {
				if message, ok := publicAgentPart(output); ok && (!background || forHuman(message.Text)) {
					emit(part, "Agent", exportText(message.Text))
				}
			}
		}
		return true
	})
}

func CopyChoices(ctx context.Context, r Reader, id session.ID, through sessionstore.Sequence) ([]MessageChoice, error) {
	var choices []MessageChoice
	err := walkCopyMessages(ctx, r, id, through, func(choice MessageChoice, _ string) { choices = append(choices, choice) })
	if err != nil {
		return nil, exportFailure(ctx, err, "history_unavailable")
	}
	for i, j := 0, len(choices)-1; i < j; i, j = i+1, j-1 {
		choices[i], choices[j] = choices[j], choices[i]
	}
	return choices, nil
}

func CopyMessagesText(ctx context.Context, r Reader, id session.ID, choices []MessageChoice) (string, error) {
	if len(choices) == 0 {
		return "", &ExportError{Code: "no_conversation"}
	}
	type reference struct {
		sequence sessionstore.Sequence
		part     int
	}
	selected := map[reference]bool{}
	var through sessionstore.Sequence
	for _, choice := range choices {
		selected[reference{choice.Sequence, choice.Part}] = true
		through = max(through, choice.Sequence)
	}
	var bodies []string
	err := walkCopyMessages(ctx, r, id, through, func(choice MessageChoice, text string) {
		key := reference{choice.Sequence, choice.Part}
		if !selected[key] {
			return
		}
		delete(selected, key)
		if len(choices) > 1 {
			text = "## " + choice.Role + "\n\n" + text
		}
		bodies = append(bodies, text)
	})
	if err != nil {
		return "", exportFailure(ctx, err, "history_unavailable")
	}
	if len(selected) > 0 {
		return "", &ExportError{Code: "response_unavailable"}
	}
	return strings.Join(bodies, "\n\n"), nil
}

func copyPicker(choices []MessageChoice) *Picker {
	p := &Picker{Kind: "copy", Title: "Copy to the clipboard", Hint: "↑↓ move · Space select · Enter copy", Messages: choices, Checked: map[int]bool{}, Options: []string{"すべて"}}
	for _, choice := range choices {
		p.Options = append(p.Options, fmt.Sprintf("%s  %s", choice.Role, choice.Preview))
	}
	return p
}

func (p *Picker) toggleCopy() {
	if p.Checked[p.Selection] {
		delete(p.Checked, p.Selection)
		return
	}
	if p.Selection == 0 {
		clear(p.Checked)
	} else {
		delete(p.Checked, 0)
	}
	p.Checked[p.Selection] = true
}

func (p *Picker) copySelection() []MessageChoice {
	if p.Checked[0] || len(p.Checked) == 0 && p.Selection == 0 {
		return p.Messages
	}
	var selected []MessageChoice
	for i, choice := range p.Messages {
		if p.Checked[i+1] || len(p.Checked) == 0 && p.Selection == i+1 {
			selected = append(selected, choice)
		}
	}
	return selected
}
