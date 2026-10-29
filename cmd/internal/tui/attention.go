package tui

import (
	"fmt"
	"github.com/unreallabsai/unreal-agent/internal/usertext"
	"strings"
)

// attentionBand is drawn across both panes, directly above the keybar. It uses
// the current conversation, independent of a scrolled transcript page.
func attentionBand(s Snapshot, u UIState, l layout) []line {
	type notice struct {
		text string
		role colorRole
	}
	var notices []notice
	if s.Failure != "" || strings.Contains(s.Status, "failed") {
		reason := s.Failure
		if reason == "" {
			reason = strings.TrimPrefix(s.Status, "session failed: ")
		}
		provider := ""
		if s.Selection != nil {
			provider = s.Selection.Provider
		}
		notices = append(notices, notice{"✗ " + usertext.Failure("", reason, provider), bandBad})
	}
	approvalOpen := u.Picker != nil && (u.Picker.Kind == "permissions" || u.Picker.Kind == "permission-once")
	if n := len(s.Permissions.Pending); n > 0 && !approvalOpen {
		notices = append(notices, notice{fmt.Sprintf("▲ 承認待ち %d件  /permissions", n), bandWarn})
	}
	messages := s.Attention
	if !s.LazyHistory {
		messages = nil
		for _, e := range s.Entries {
			if e.Role == "you" {
				messages = nil
			}
			if e.Role == "agent" && strings.HasPrefix(strings.TrimSpace(e.Text), "【要確認】") {
				messages = append(messages, firstLine(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e.Text), "【要確認】"))))
			}
		}
	}
	for _, message := range messages {
		notices = append(notices, notice{"▲ 要確認  " + message + "  ↑ 会話で確認", bandWarn})
	}
	if len(notices) == 0 {
		return nil
	}
	extra := ""
	if len(notices) > 1 {
		extra = fmt.Sprintf(" +%d", len(notices)-1)
	}
	n := notices[0]
	// Measure the actual fallback delimiters as well as the reserved count.
	overhead := u.Theme.roleLine(extra, n.role).width()
	text := tail(u.Theme.chrome(n.text), max(0, l.width-overhead), u.Theme) + extra
	return []line{u.Theme.roleLine(text, n.role).clip(l.width)}
}

func firstLine(text string) string {
	return strings.SplitN(SafeText(text), "\n", 2)[0]
}
