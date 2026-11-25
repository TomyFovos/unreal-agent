package contextengine

import (
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// ForProvider overlays a resolved config using the exact runtime provider ID.
func (c Config) ForProvider(provider string) Config {
	return c.ForRole(provider, "")
}

// ForRole resolves the common, provider and child role policy in that order.
func (c Config) ForRole(provider, role string) Config {
	r := c.Routes[provider]
	c.Routes = nil
	c.applyOverrides(r)
	if overrides, ok := r.Roles[role]; role != "" && ok {
		c.applyOverrides(overrides)
	}
	return c
}

func (c *Config) applyOverrides(r RouteConfig) {
	override(&c.FallbackWindow, r.FallbackWindow)
	override(&c.InputBudget, r.InputBudget)
	override(&c.ResponseReserve, r.ResponseReserve)
	override(&c.ProtocolReserve, r.ProtocolReserve)
	override(&c.WindowThresholdTokens, r.WindowThresholdTokens)
	override(&c.PostRebuildRatio, r.PostRebuildRatio)
	override(&c.L2TargetTokens, r.L2TargetTokens)
	override(&c.RecentEventCount, r.RecentEventCount)
	override(&c.ToolOutputExcerptChars, r.ToolOutputExcerptChars)
	override(&c.CacheRewarmMinutes, r.CacheRewarmMinutes)
}

func override[T int | int64 | float64](value, replacement *T) {
	if replacement != nil {
		*value = *replacement
	}
}

// RebuildTarget is the soft target. The model's hard input budget can make
// it smaller than the nominal target used to validate the configured L2 cap.
func (c Config) RebuildTarget(inputBudget int64) int64 {
	return max(1, int64(float64(min(c.WindowThresholdTokens, inputBudget))*c.PostRebuildRatio))
}

func (c Config) validatePolicy() error {
	if c.WindowThresholdTokens <= 0 {
		return fmt.Errorf("WindowThresholdTokens must be positive")
	}
	if !(0 < c.PostRebuildRatio && c.PostRebuildRatio < 1) {
		return fmt.Errorf("PostRebuildRatio must be between 0 and 1 (exclusive)")
	}
	target := c.RebuildTarget(c.WindowThresholdTokens)
	if c.L2TargetTokens < 0 || c.L2TargetTokens > target {
		return fmt.Errorf("L2TargetTokens must be between 0 and the rebuild target (%d)", target)
	}
	if c.RecentEventCount < 0 || c.ToolOutputExcerptChars < 0 || c.CacheRewarmMinutes < 0 {
		return &Error{"invalid_configuration"}
	}
	if c.InputBudget <= 0 || c.ResponseReserve < 0 || c.ProtocolReserve < 0 ||
		c.FallbackWindow <= c.ResponseReserve || c.FallbackWindow-c.ResponseReserve <= c.ProtocolReserve {
		return &Error{"invalid_budget"}
	}
	return nil
}

func (c Config) Policies() sessionstore.ContextPolicies {
	return c.PoliciesForRole("")
}

func (c Config) PoliciesForRole(role string) sessionstore.ContextPolicies {
	policies := sessionstore.ContextPolicies{Role: role, Default: c.windowPolicy()}
	if len(c.Routes) > 0 {
		policies.Routes = make(map[string]sessionstore.WindowPolicy, len(c.Routes))
		for provider := range c.Routes {
			policies.Routes[provider] = c.ForRole(provider, role).windowPolicy()
		}
	}
	return policies
}

func (c Config) windowPolicy() sessionstore.WindowPolicy {
	return sessionstore.WindowPolicy{
		WindowThresholdTokens: c.WindowThresholdTokens, PostRebuildRatio: c.PostRebuildRatio, L2TargetTokens: c.L2TargetTokens,
		RecentEventCount: c.RecentEventCount, ToolOutputExcerptChars: c.ToolOutputExcerptChars, CacheRewarmMinutes: c.CacheRewarmMinutes,
		FallbackWindow: c.FallbackWindow, InputBudget: c.InputBudget, ResponseReserve: c.ResponseReserve, ProtocolReserve: c.ProtocolReserve,
	}
}
