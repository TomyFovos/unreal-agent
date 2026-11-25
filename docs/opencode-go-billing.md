# OpenCode Go billing

[日本語](opencode-go.md#課金額と課金区分)

OpenCode Go reports billing alongside tokens in the Responses `usage.opencode` block. Unreal records `allowance` as the USD amount drawn from the allowance, `list_cost` as the USD list amount and `billing_source` as the provider’s billing label. Older Session records supply the same fields through `Usage.Raw.opencode`. An absent amount stays unknown.

On November 24, 2026, one observed Session shifted from `go` to `go-plus`, and its reported allowance-to-list ratio rose from 0.60 to 1.20. Unreal reads the reported amount for each response. Run [`unreal-agent usage`](usage.md#inspecting-session-usage) or open **Usage** in `/analyze` to see charges by billing source and compare the cost per call.
