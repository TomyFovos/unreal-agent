package analysis

import (
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// Offline, versioned list-price equivalents, not subscription charges. Source:
// harness/modelcatalog/testdata/opencode-go-models-2026-11-05.json (public /models).
// Only exact known identities are priced; new models/providers stay unknown.
const PriceTableSource = "OpenCode Go /models snapshot 2026-11-05; USD per million tokens"

func listPrice(model ModelKey, u llm.Usage, at time.Time) (float64, bool) {
	rates, ok := listPriceRates(model, at)
	if !ok || rates.write == 0 && u.CacheWriteInputTokens > 0 {
		return 0, false
	}
	tokens := llm.ReportedTokens(u)
	if !tokens.InputKnown || !tokens.OutputKnown || slices.Contains(u.Unknown, llm.UsageCachedInput) || slices.Contains(u.Unknown, llm.UsageCacheWriteInput) || u.CachedInputTokens < 0 || u.CacheWriteInputTokens < 0 || u.CachedInputTokens+u.CacheWriteInputTokens > tokens.Input {
		return 0, false
	}
	return (float64(tokens.Input-u.CachedInputTokens-u.CacheWriteInputTokens)*rates.input + float64(u.CachedInputTokens)*rates.read + float64(u.CacheWriteInputTokens)*rates.write + float64(tokens.Output)*rates.output) / 1e6, true
}

type tokenPrices struct{ input, read, write, output float64 }

func listPriceRates(model ModelKey, at time.Time) (tokenPrices, bool) {
	if model.Provider != "opencode-go" {
		return tokenPrices{}, false
	}
	var input, read, write, output float64
	switch model.Model {
	case "deepseek-v4.1-flash":
		input, read, output = .15, .003, .6
		if at.IsZero() {
			return tokenPrices{}, false
		}
		// The snapshot explicitly doubles weekday UTC 01–04 and 06–10.
		t := at.UTC()
		if t.Weekday() != time.Saturday && t.Weekday() != time.Sunday && (t.Hour() >= 1 && t.Hour() < 4 || t.Hour() >= 6 && t.Hour() < 10) {
			input *= 2
			read *= 2
			output *= 2
		}
	case "qwen3.8-flash":
		input, read, write, output = .15, .016, .2, .47
	default:
		return tokenPrices{}, false
	}
	return tokenPrices{input, read, write, output}, true
}
