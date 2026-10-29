package viewer

import (
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"testing"
)

func TestCacheLabelUsesReportedInputAndCachedCounts(t *testing.T) {
	for _, test := range []struct {
		usage Usage
		want  string
	}{
		{Usage{Known: true, Input: 100, CachedInput: 95}, "cache 95.00%"},
		{Usage{Known: true, Input: 100}, "cache 0.00%"},
		{Usage{Known: true, Input: 10000, CachedInput: 9837}, "cache 98.37%"},
		{Usage{Known: true, Input: 100, CachedInput: 95, Unknown: []llm.UsageField{llm.UsageCachedInput}}, "cache -"},
		{Usage{}, "cache -"},
		{Usage{Known: true}, "cache -"},
	} {
		if got := CacheLabel(test.usage); got != test.want {
			t.Fatalf("%s want %s", got, test.want)
		}
	}
}
