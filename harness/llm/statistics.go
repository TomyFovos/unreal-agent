package llm

import "slices"

// TokenCounts contains only explicitly reported counters. It excludes raw
// provider usage, context estimates, text, identifiers and reasoning bodies.
type TokenCounts struct {
	Input, Output           int64
	InputKnown, OutputKnown bool
}

func ReportedTokens(u Usage) TokenCounts {
	// A zero-value legacy/failing response carries no evidence of measured zero.
	reported := len(u.Raw) > 0 || u.InputTokens > 0 || u.OutputTokens > 0
	return TokenCounts{Input: u.InputTokens, Output: u.OutputTokens,
		InputKnown:  reported && u.InputTokens >= 0 && !slices.Contains(u.Unknown, UsageInput),
		OutputKnown: reported && u.OutputTokens >= 0 && !slices.Contains(u.Unknown, UsageOutput)}.Safe()
}

func (t TokenCounts) Safe() TokenCounts {
	if !t.InputKnown || t.Input < 0 {
		t.Input, t.InputKnown = 0, false
	}
	if !t.OutputKnown || t.Output < 0 {
		t.Output, t.OutputKnown = 0, false
	}
	return t
}

// GenerationStatistics describes one generation, not turn/Operation wall time.
// provider_api is the provider's API duration. client_clock is an explicitly
// approximate request interval, excluding tool execution and approval waits.
// Tokens are always reported counters; no text-to-token estimate is made.
type GenerationStatistics struct {
	Model          string
	Tokens         TokenCounts
	DurationMicros int64
	Source         string
}

func (s GenerationStatistics) Valid() bool {
	return (s.Source == "provider_api" || s.Source == "client_clock") && s.DurationMicros > 0 && s.DurationMicros <= 24*60*60*1000000 && s.Tokens == s.Tokens.Safe()
}

func (s GenerationStatistics) OutputTPS() (float64, bool) {
	if !s.Valid() || !s.Tokens.OutputKnown {
		return 0, false
	}
	return float64(s.Tokens.Output) * 1e6 / float64(s.DurationMicros), true
}
