package llm

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestGenerationStatisticsReportedUnknownAndSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage Usage
		want  TokenCounts
	}{
		{"legacy-absent", Usage{}, TokenCounts{}},
		{"reported-zero", Usage{Raw: []byte(`{"input_tokens":0,"output_tokens":0}`)}, TokenCounts{InputKnown: true, OutputKnown: true}},
		{"partial", Usage{OutputTokens: 8, Unknown: []UsageField{UsageInput}}, TokenCounts{Output: 8, OutputKnown: true}},
		{"negative", Usage{InputTokens: -1, OutputTokens: 8}, TokenCounts{Output: 8, OutputKnown: true}},
		{"complete", Usage{InputTokens: 100, OutputTokens: 20}, TokenCounts{Input: 100, Output: 20, InputKnown: true, OutputKnown: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReportedTokens(tc.usage); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
	for _, source := range []string{"provider_api", "client_clock", "invented", ""} {
		s := GenerationStatistics{Tokens: TokenCounts{Output: 20, OutputKnown: true}, DurationMicros: 2_000_000, Source: source}
		value, known := s.OutputTPS()
		valid := source == "provider_api" || source == "client_clock"
		if known != valid || valid && value != 10 {
			t.Fatal(source, value, known)
		}
		for _, duration := range []int64{0, -1, 24*60*60*1_000_000 + 1} {
			s.DurationMicros = duration
			if _, known := s.OutputTPS(); known {
				t.Fatal("invalid duration made a rate", duration)
			}
		}
	}
	response := Response{Generation: &GenerationStatistics{Source: "provider_api", DurationMicros: 2_000_000, Tokens: TokenCounts{Output: 20, OutputKnown: true}}}
	encoded, err := json.Marshal(response)
	var restored Response
	if err != nil || json.Unmarshal(encoded, &restored) != nil || *restored.Generation != *response.Generation {
		t.Fatal("statistics did not roundtrip", err)
	}
	legacy, _ := json.Marshal(Response{})
	if strings.Contains(string(legacy), "Generation") {
		t.Fatal("changed legacy response shape")
	}
}
