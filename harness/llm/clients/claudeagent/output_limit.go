package claudeagent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type outputLimitHit struct {
	tokens int64
	phase  string
}

var outputLimitMessage = regexp.MustCompile(`response exceeded the ([0-9]+) output token maximum`)

// Recovery belongs to Claude Code. Only replace its final failure explanation;
// never retry, change effort or suggest increasing a model's output ceiling.
func (s *streamActivities) outputLimitFailure(result *frame, message string) string {
	match := outputLimitMessage.FindStringSubmatch(message)
	if result.StopReason != "max_output_tokens" && result.StopReason != "max_tokens" && len(match) == 0 {
		return message
	}
	s.mu.Lock()
	hits := append([]outputLimitHit(nil), s.limits[""]...)
	s.mu.Unlock()
	var tokens int64
	if len(match) > 1 {
		tokens, _ = strconv.ParseInt(match[1], 10, 64)
	}
	thinking, text := 0, 0
	for _, hit := range hits {
		if tokens == 0 {
			tokens = hit.tokens
		}
		switch hit.phase {
		case "thinking":
			thinking++
		case "text":
			text++
		}
	}
	limit := "the output token limit"
	if tokens > 0 {
		limit = fmt.Sprintf("the output limit (%d tokens)", tokens)
	}
	frequency := "at least once (no stream count was available)"
	if len(hits) > 0 {
		frequency = fmt.Sprintf("%d times", len(hits))
		if len(hits) == 1 {
			frequency = "1 time"
		}
		var phases []string
		if thinking > 0 {
			phases = append(phases, fmt.Sprintf("thinking: %d", thinking))
		}
		if text > 0 {
			phases = append(phases, fmt.Sprintf("text: %d", text))
		}
		if unknown := len(hits) - thinking - text; unknown > 0 {
			phases = append(phases, fmt.Sprintf("phase unknown: %d", unknown))
		}
		frequency += " (" + strings.Join(phases, ", ") + ")"
	}
	return "Claude Code reached " + limit + " " + frequency + ". Its recovery attempts did not complete the response. Lowering effort or splitting the work among child agents may allow it to continue."
}
