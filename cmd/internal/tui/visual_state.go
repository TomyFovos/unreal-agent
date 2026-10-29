package tui

import (
	"strings"
	"unicode"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// visualState is presentation only. Canonical operation and Finish semantics
// remain with Host; legacy renderers migrate to this type in later steps.
type visualState uint8

const (
	stateUnknown visualState = iota
	stateWorking
	stateCanceling
	stateApproval
	stateCompleted
	stateFailed
	stateCanceled
	stateEnded
)

func operationVisualState(status operation.Status) visualState {
	switch status {
	case operation.StatusAwaiting, operation.StatusReady:
		return stateWorking
	case operation.StatusCanceling:
		return stateCanceling
	case operation.StatusCompleted:
		return stateCompleted
	case operation.StatusFailed:
		return stateFailed
	case operation.StatusCanceled:
		return stateCanceled
	default:
		return stateUnknown
	}
}

func childVisualState(r viewer.Row, s Snapshot) visualState {
	if !s.Connected || r.NeedsResync {
		return stateUnknown
	}
	if r.Finish != nil {
		switch r.Finish.Status {
		case "success", "completed":
			return stateCompleted
		case "failure", "failed", "error":
			return stateFailed
		default:
			return stateUnknown
		}
	}
	if r.ParentOperationStatus == operation.StatusCompleted {
		return stateEnded
	}
	// Only a parent known to be stopped ends its children's display; an
	// unknown parent (a brief re-sync) must not hide a running child.
	if r.ParentRuntime == viewer.RuntimeStopped {
		switch r.ParentOperationStatus {
		case operation.StatusCanceled, operation.StatusCanceling:
			return stateCanceled
		default:
			return stateEnded
		}
	}
	return operationVisualState(r.ParentOperationStatus)
}

func toolVisualState(r Receipt, s Snapshot) visualState {
	if !s.Connected {
		return stateUnknown
	}
	if r.Status == operation.StatusAwaiting {
		for _, pending := range s.Permissions.Pending {
			for _, id := range r.Operations {
				if pending.ID == string(id) {
					return stateApproval
				}
			}
		}
	}
	return operationVisualState(r.Status)
}

func (s visualState) presentation(t Theme) (mark, word string, color style) {
	role := stateStall
	switch s {
	case stateWorking:
		mark, word, role = t.symbol("▸", ">"), "作業中", stateRun
	case stateCanceling:
		mark, word = t.symbol("▸", ">"), "取消中"
	case stateApproval:
		mark, word = "!", "承認待ち"
	case stateCompleted:
		mark, word, role = t.symbol("✓", "+"), "完了", stateDone
	case stateFailed:
		mark, word, role = t.symbol("✗", "x"), "失敗", stateBad
	case stateCanceled:
		mark, word, role = t.symbol("⊘", "-"), "取消", dimColor
	case stateEnded:
		mark, word, role = t.symbol("▪", "#"), "終了", dimColor
	default:
		mark, word = "?", "不明"
	}
	return mark, word, visualStyle(role)
}

func (s visualState) ended() bool {
	return s == stateCompleted || s == stateFailed || s == stateCanceled || s == stateEnded
}

// displayTool expands the official Claude Code wrapper once for all future
// chips, pulse and cards. Keep the remaining target (including command spacing).
func displayTool(name, target string) (string, string) {
	if name != "claude_code" {
		return name, target
	}
	target = strings.TrimSpace(target)
	if i := strings.IndexFunc(target, unicode.IsSpace); i >= 0 {
		return target[:i], strings.TrimSpace(target[i:])
	}
	return target, ""
}
