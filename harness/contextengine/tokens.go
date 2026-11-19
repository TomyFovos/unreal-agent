package contextengine

import (
	"encoding/json/v2"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/claudeprompt"
)

// TokenEstimator estimates model-visible content in tokens, independently of
// transport JSON escaping, provider IDs and opaque encrypted reasoning. These
// are estimates, never provider-reported counts. All profiles use the same
// text heuristic: four ASCII characters per token, one non-ASCII rune per token.
// No admission margin is added. Provider profiles select the content actually
// sent by that transport; compatible API clients share the Responses profile.
type TokenEstimator struct{ Provider string }

func (e TokenEstimator) Claude() bool {
	return e.Provider == "claude" || e.Provider == "claude-code" || e.Provider == "claude-api"
}

func (e TokenEstimator) Profile() string {
	switch e.Provider {
	case "claude", "claude-code", "claude-api":
		return "claudeagent-content"
	default:
		return "responsesapi-content:" + e.Provider
	}
}

// TextSize is additive before the ASCII token count is rounded. Use it for
// prompt fragments joined into one string.
type TextSize struct{ ASCII, Other int64 }

func MeasureText(text string) TextSize {
	var size TextSize
	for _, r := range text {
		if r < utf8.RuneSelf {
			size.ASCII++
		} else {
			size.Other++
		}
	}
	return size
}

func (s TextSize) Tokens() int64 { return (s.ASCII+3)/4 + s.Other }

func (e TokenEstimator) Text(text string) int64 { return MeasureText(text).Tokens() }

func (e TokenEstimator) Items(items []llm.Item) int64 {
	var tokens int64
	for _, item := range items {
		tokens += e.Item(item)
	}
	return tokens
}

func (e TokenEstimator) Item(item llm.Item) int64 {
	switch v := item.Data.(type) {
	case llm.Message:
		if e.Claude() && v.Role == llm.RoleSystem {
			// claudeagent.planPrompt omits Unreal's system message.
			return 0
		}
		return e.Text(v.Text)
	case llm.ToolCall:
		return e.Text(v.Name) + e.Text(v.Arguments)
	case llm.ToolResult:
		return e.ToolResult(v)
	case llm.Reasoning:
		if e.Claude() {
			return 0
		}
		// Responses replays Raw verbatim. Only its public summary is text;
		// ciphertext length does not tell us its underlying reasoning tokens.
		if len(v.Raw) > 0 {
			var raw struct {
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
			}
			if json.Unmarshal(v.Raw, &raw) == nil {
				var tokens int64
				for _, part := range raw.Summary {
					tokens += e.Text(part.Text)
				}
				return tokens
			}
		}
		return e.Text(strings.Join(v.Summary, "\n"))
	}
	return 0
}

func (e TokenEstimator) ToolResult(result llm.ToolResult) int64 {
	var tokens int64
	for _, part := range result.Output {
		switch part.Kind {
		case llm.ToolResultText, llm.ToolResultImage:
			// Without reported image usage this is a content-size heuristic, not
			// the provider's image tokenizer or a charge for the JSON envelope.
			tokens += e.Text(part.Value)
		}
	}
	return tokens
}

func (e TokenEstimator) Tools(tools []llm.Tool) int64 {
	if e.Claude() {
		// Claude Code supplies its own tools, not Request.Tools.
		return 0
	}
	var tokens int64
	for _, tool := range tools {
		tokens += e.Text(tool.Name) + e.Text(tool.Description)
		if len(tool.Parameters) > 0 {
			data, err := json.Marshal(tool.Parameters, json.Deterministic(true))
			if err == nil {
				tokens += e.Text(string(data))
			}
			// Invalid schemas are rejected by the encoder, not as context overflow.
		}
	}
	return tokens
}

func (e TokenEstimator) Request(request llm.Request) int64 {
	if e.Claude() {
		// Full includes the historical context already held by a resumed CLI.
		// New alone is just the appended transport text, not its total context.
		return e.Text(claudeprompt.Plan(request.Input).Full)
	}
	return e.Items(request.Input) + e.Tools(request.Tools)
}
