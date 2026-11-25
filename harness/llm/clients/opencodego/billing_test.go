package opencodego

import (
	"encoding/json/jsontext"
	"testing"
)

func TestReportedChargePreservesSourceAndMissingAmounts(t *testing.T) {
	for _, tc := range []struct {
		raw, source   string
		charged, list *float64
	}{
		{`{"opencode":{"billing_source":"go","allowance":0.6,"list_cost":1}}`, "go", new(0.6), new(1.0)},
		{`{"opencode":{"billing_source":"go-plus","allowance":1.2,"list_cost":1}}`, "go-plus", new(1.2), new(1.0)},
		{`{"opencode":{"billing_source":"go-plus","allowance":0}}`, "go-plus", new(0.0), nil},
		{`{"opencode":{"billing_source":"go","allowance":null,"list_cost":1}}`, "go", nil, new(1.0)},
		{`{"opencode":{"billing_source":"go"},"cost":0}`, "", nil, nil},
		{`{"input_tokens":42}`, "", nil, nil},
	} {
		got := ReportedCharge(jsontext.Value(tc.raw))
		if tc.source == "" {
			if got != nil {
				t.Fatalf("expected unknown charge for %s, got %+v", tc.raw, got)
			}
			continue
		}
		same := func(a, b *float64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
		if got == nil || got.Currency != "USD" || got.BillingSource != tc.source || !same(got.Charged, tc.charged) || !same(got.List, tc.list) {
			t.Fatalf("charge for %s = %+v", tc.raw, got)
		}
	}
}
