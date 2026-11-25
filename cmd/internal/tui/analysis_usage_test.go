package tui

import (
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
)

func TestAnalysisUsageShowsModelCallsAndInputPerCall(t *testing.T) {
	r := analysisFixture()
	r.RoleUsage = []analysis.RoleUsage{{Role: "lead", Calls: analysis.Metric{Known: true, Value: 2142},
		InputTokensPerCall: analysis.Metric{Known: true, Value: 166_000, Unknown: 1}}}
	got := analysisText(r, "Usage", 100, false)
	if !strings.Contains(got, "By role") || !strings.Contains(got, "lead") ||
		!strings.Contains(got, "2142 model calls · ~166.0k input tokens/call") {
		t.Fatalf("missing role call usage:\n%s", got)
	}
}
