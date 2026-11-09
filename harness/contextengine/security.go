package contextengine

import (
	"encoding/json/v2"
	"regexp"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

// Eligibility is an allowlist of public payload types, followed by conservative
// secret-pattern exclusion. Configurations, controls, reasoning, usage.Raw,
// provider IDs, credentials and process environments have no ingestion path.
var privateSource = regexp.MustCompile(`(?i)(\.codex[/\\]auth\.json|\.claude[/\\]\.credentials|"(?:env|environment|credentials|authentication)"\s*:|\b(?:printenv|set -o posix|declare -x)\b)`)

func Sensitive(text string) bool { return secretguard.Credentials(text) }

// PublicItem drops provider-private fields and redacts credential-shaped text.
// Calls reading private sources are excluded together with their receipts.
func PublicItem(item llm.Item) (llm.Item, bool) {
	var data any
	switch v := item.Data.(type) {
	case llm.Message:
		if item.Type != llm.ItemMessage || v.Role != llm.RoleUser && v.Role != llm.RoleAssistant && v.Role != llm.RoleSystem || v.Phase != "" && v.Phase != "commentary" && v.Phase != "final" && v.Phase != "final_answer" {
			return llm.Item{}, false
		}
		v.Text = secretguard.Redact(v.Text)
		data = v
	case llm.ToolCall:
		if item.Type != llm.ItemToolCall || privateSource.MatchString(v.Arguments) {
			return llm.Item{}, false
		}
		v.Arguments = secretguard.Redact(v.Arguments)
		v.Name = secretguard.Redact(v.Name)
		data = v
	case llm.ToolResult:
		if item.Type != llm.ItemToolResult {
			return llm.Item{}, false
		}
		v.Output = append([]llm.ToolResultOutput(nil), v.Output...)
		for i, o := range v.Output {
			if o.Kind != llm.ToolResultText && o.Kind != llm.ToolResultImage {
				return llm.Item{}, false
			}
			v.Output[i].Value = secretguard.Redact(o.Value)
		}
		data = v
	default:
		return llm.Item{}, false
	}
	return llm.Item{Type: item.Type, Data: data}, true
}

const (
	PartialRedaction = "partial_redaction"
	PrivateSource    = "private_source"
	NonPublic        = "non_public"
)

// PublicItemWithRedaction uses the same guard, with body-free measurement of
// changed/excluded payloads. Provider-private fields are not measured.
func PublicItemWithRedaction(item llm.Item) (llm.Item, bool, *llm.ContextRedaction) {
	public, ok := PublicItem(item)
	if ok {
		changed := false
		switch v := item.Data.(type) {
		case llm.Message:
			changed = v.Text != public.Data.(llm.Message).Text
		case llm.ToolCall:
			p := public.Data.(llm.ToolCall)
			changed = v.Name != p.Name || v.Arguments != p.Arguments
		case llm.ToolResult:
			p := public.Data.(llm.ToolResult)
			for i, part := range v.Output {
				changed = changed || part.Value != p.Output[i].Value
			}
		}
		if !changed {
			return public, true, nil
		}
	}
	r := &llm.ContextRedaction{Reason: PartialRedaction, BeforeBytes: PublicPayloadBytes(item)}
	switch v := item.Data.(type) {
	case llm.ToolCall:
		r.CallID = v.CallID
	case llm.ToolResult:
		r.CallID = v.CallID
	}
	if ok {
		r.AfterBytes = PublicPayloadBytes(public)
	} else {
		r.Reason = NonPublic
		if v, call := item.Data.(llm.ToolCall); call && item.Type == llm.ItemToolCall && privateSource.MatchString(v.Arguments) {
			r.Reason = PrivateSource
		}
	}
	return public, ok, r
}

// PublicPayloadBytes counts UTF-8 payload bytes, not tokens or JSON framing.
func PublicPayloadBytes(item llm.Item) int64 {
	switch v := item.Data.(type) {
	case llm.Message:
		return int64(len(v.Text))
	case llm.ToolCall:
		return int64(len(v.Name) + len(v.Arguments))
	case llm.ToolResult:
		var n int64
		for _, part := range v.Output {
			n += int64(len(part.Value))
		}
		return n
	default:
		data, _ := json.Marshal(item.Data)
		return int64(len(data))
	}
}

// Estimate deliberately uses UTF-8/JSON bytes, not the optimistic chars/4 rule.
// It includes escaping, per-item framing and a margin; it is still an estimate.
func Estimate(v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 1 << 60
	}
	return int64(len(b)) + 64
}

func Text(item llm.Item) string {
	switch v := item.Data.(type) {
	case llm.Message:
		return v.Text
	case llm.ToolCall:
		return v.Name + " " + v.Arguments
	case llm.ToolResult:
		var text []string
		for _, o := range v.Output {
			if o.Kind == llm.ToolResultText {
				text = append(text, o.Value)
			}
		}
		return strings.Join(text, "\n")
	}
	return ""
}

// Portable renders historical tools as quoted data, never executable calls.
func Portable(u Unit) llm.Item {
	item := u.Item
	if _, ok := item.Data.(llm.Message); !ok {
		b, _ := json.Marshal(item.Data)
		role := llm.RoleAssistant
		if u.Kind == ToolResult || u.Kind == ChildResult {
			role = llm.RoleUser
		}
		item = llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: role, Text: "Prior Unreal " + string(u.Kind) + " (historical data):\n" + string(b)}}
	}
	return item
}
