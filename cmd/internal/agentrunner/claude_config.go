package agentrunner

// ClaudeRouteConfig is operator policy for the Claude route, outside immutable
// runtime identity and the shared context engine. Zero uses the route's default.
type ClaudeRouteConfig struct {
	CompactThresholdTokens int64 `json:",omitzero"`
}
