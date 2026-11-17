package provider

import (
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
)

func TestNormalizeUsageLimitPreservesOnlySafeReset(t *testing.T) {
	until := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	err := normalize(&responsesapi.APIError{StatusCode: 429, Type: "usage_limit_reached", Message: "private-body", ResetAt: until})
	limited, ok := err.(llm.UsageLimitFailure)
	if !ok {
		t.Fatal("lost usage limit classification", err)
	}
	if at, reached := limited.UsageLimitReset(); !reached || !at.Equal(until) {
		t.Fatal(at, reached)
	}
	if strings.Contains(err.Error(), "private") || err.(*Error).RetryableModelFailure() {
		t.Fatal(err)
	}
}
