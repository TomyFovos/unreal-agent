package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

// The footer changes only with provider counters or canonical responses. No
// rate timer runs through Operation/approval waits.
func statisticsBar(s Snapshot, u UIState, l layout) line {
	unknown := u.Theme.symbol("—", "-")
	stats := currentStatistics(s)
	contextNumber, rate := unknown, unknown
	contextBar, speedBars, cache := line(nil), line(nil), textLine("キャッシュ "+unknown, meta)
	if stats != nil {
		if input, known := stats.ContextInput(); known {
			contextNumber = strings.ReplaceAll(strings.ReplaceAll(number(input), ".0k", "k"), ".0m", "m")
			if s.Selection != nil && s.Selection.ContextWindow > 0 {
				ratio := float64(input) / float64(s.Selection.ContextWindow)
				contextNumber += fmt.Sprintf(" (%.0f%%)", ratio*100)
				role := stateMain
				if ratio >= .9 {
					role = stateBad
				} else if ratio >= .7 {
					role = stateStall
				}
				contextBar = gaugeBar(ratio, 12, role, u.Theme)
			}
		}
		if value, known := stats.OutputTPS(); known {
			prefix := ""
			if stats.Source != "provider_api" {
				prefix = "~"
			}
			rate = fmt.Sprintf("%s%.1f tok/s", prefix, value)
		}
	}
	speedBars = generationBars(s, u.Theme)
	if label := viewer.CacheLabel(s.CacheLatest); label != "cache -" {
		ratio := float64(s.CacheLatest.CachedInput) / float64(s.CacheLatest.Input)
		cache = joined(textLine("キャッシュ ", meta), gaugeBar(ratio, 10, stateDone, u.Theme), textLine(strings.TrimPrefix(label, "cache "), meta))
	}
	build := func() line {
		out := textLine("文脈 ", meta)
		out = joined(out, contextBar, textLine(contextNumber, meta), textLine(u.Theme.symbol(" · ", " / ")+"速度 ", meta), speedBars, textLine(rate, meta))
		if cache != nil {
			out = joined(out, textLine(u.Theme.symbol(" · ", " / "), meta), cache)
		}
		return out
	}
	if build().width() > l.width {
		cache = nil
	}
	if build().width() > l.width {
		speedBars = nil
	}
	if build().width() > l.width {
		contextBar = nil
	}
	return build().clip(l.width)
}

func currentStatistics(s Snapshot) *llm.GenerationStatistics {
	stats := s.LastGeneration
	if s.Progress != nil && s.Progress.Statistics != nil {
		stats = s.Progress.Statistics
	} else if s.Progress != nil && !s.Progress.Done {
		return nil
	}
	if stats != nil && stats.Model != "" && s.Selection != nil && stats.Model != s.Selection.Model {
		return nil
	}
	return stats
}

func gaugeBar(ratio float64, cells int, role colorRole, t Theme) line {
	filled := max(0, min(cells, int(math.Round(ratio*float64(cells)))))
	return textLine(t.symbol("▕", "|")+t.chrome(strings.Repeat("█", filled)+strings.Repeat("░", cells-filled))+t.symbol("▏", "|"), visualStyle(role))
}

func generationBars(s Snapshot, t Theme) line {
	history := append([]llm.GenerationStatistics(nil), s.SpeedHistory...)
	if stats := currentStatistics(s); stats != nil && (len(history) == 0 || s.Progress != nil && s.Progress.Statistics != nil) {
		history = append(history, *stats)
	}
	if len(history) > 8 {
		history = history[len(history)-8:]
	}
	maximum := 0.0
	for _, stats := range history {
		if rate, known := stats.OutputTPS(); known {
			maximum = max(maximum, rate)
		}
	}
	var out line
	for _, stats := range history {
		rate, known := stats.OutputTPS()
		mark := t.symbol("—", "-")
		if known {
			level := 0
			if maximum > 0 {
				level = int(math.Round(rate / maximum * 6))
			}
			mark = string([]rune("▁▂▃▄▅▆▇")[level])
			if t.ASCII {
				mark = string([]rune(".:--==#")[level])
			}
		}
		out = joined(out, textLine(mark, visualStyle(stateMain)))
	}
	if len(out) > 0 {
		out = joined(out, textLine(" ", normal))
	}
	return out
}
