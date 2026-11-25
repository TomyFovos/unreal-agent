package opencodego

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// ReportedCharge decodes both live Responses usage and the raw usage retained
// by older Session records. The multiplier is defined by the provider.
func ReportedCharge(raw jsontext.Value) *llm.Charge {
	var usage struct {
		OpenCode *struct {
			Allowance     *float64 `json:"allowance"`
			ListCost      *float64 `json:"list_cost"`
			BillingSource string   `json:"billing_source"`
		} `json:"opencode"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &usage) != nil || usage.OpenCode == nil {
		return nil
	}
	b := usage.OpenCode
	if b.Allowance == nil && b.ListCost == nil {
		return nil
	}
	return &llm.Charge{Charged: b.Allowance, List: b.ListCost, Currency: "USD", BillingSource: b.BillingSource}
}
