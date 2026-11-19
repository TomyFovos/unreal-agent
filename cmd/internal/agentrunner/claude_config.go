package agentrunner

// ClaudeRouteConfig is operator policy for Claude Code's summary strategy.
// Zero uses the route's default.
type ClaudeRouteConfig struct {
	CompactThresholdTokens int64 `json:",omitzero"`
}
