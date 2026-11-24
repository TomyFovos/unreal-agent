package tui

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestResetDisplayUsesLocalZoneAndLabelsClocks(t *testing.T) {
	const marker = "UNREAL_TEST_DISPLAY_TIME_ZONE"
	if zone := os.Getenv(marker); zone != "" {
		reset := time.Date(2026, 11, 23, 22, 55, 12, 0, time.UTC)
		now := time.Date(2026, 11, 23, 3, 58, 0, 0, time.UTC)
		wantTime, wantFooter := "22:55 UTC", "(22:55) UTC"
		if zone == "Asia/Tokyo" {
			wantTime, wantFooter = "11/24 07:55 JST", "(07:55) JST"
		}
		if got := analysisTime(reset, now); got != wantTime {
			t.Fatalf("analysis clock = %q, want %q", got, wantTime)
		}
		window := analysis.SubscriptionWindow{SubscriptionObservation: llm.SubscriptionObservation{
			Provider: "claude", SubscriptionUsage: llm.SubscriptionUsage{Window: "five_hour", ResetsAt: reset, UsedPercent: new(80.0)},
		}}
		if got := subscriptionBar([]analysis.SubscriptionWindow{window}, Theme{}, 90, 30).plain(); !strings.Contains(got, wantFooter) {
			t.Fatalf("footer clock = %q, want %q", got, wantFooter)
		}
		return
	}
	for _, zone := range []string{"UTC", "Asia/Tokyo"} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestResetDisplayUsesLocalZoneAndLabelsClocks$")
		cmd.Env = append(os.Environ(), "TZ="+zone, marker+"="+zone)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("TZ=%s: %v\n%s", zone, err, out)
		}
	}
}
