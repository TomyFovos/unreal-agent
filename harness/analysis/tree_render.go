package analysis

import (
	"fmt"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func modelLabel(m ModelKey) string {
	known := func(s string) string {
		if s == "" {
			return "unknown"
		}
		return s
	}
	return known(m.Provider) + " / " + known(m.Model) + " / " + known(m.Effort)
}

func requestModelLabel(m ModelKey, fast string) string {
	label := modelLabel(m)
	if fast != "" {
		return label + " / fast " + fast
	}
	if m.Provider == "openai-codex" {
		return label + " / fast unknown"
	}
	return label
}

func layerOrder(layer string) string {
	switch layer {
	case "main":
		return "0"
	case "lead":
		return "1"
	case "worker":
		return "2"
	case "collector":
		return "3"
	default:
		return "4" + layer
	}
}

func metricText(m Metric, unit string) string {
	if !m.Known {
		return "unknown"
	}
	source := "measured"
	if m.Derived > 0 {
		source = "derived"
		if m.Measured > 0 {
			source = "measured+derived"
		}
	}
	partial := ""
	if m.Unknown > 0 {
		partial = fmt.Sprintf("; %d unknown", m.Unknown)
	}
	if unit == " USD" {
		return fmt.Sprintf("%.6f%s (%s%s)", m.Value, unit, source, partial)
	}
	return fmt.Sprintf("%.2f%s (%s%s)", m.Value, unit, source, partial)
}

func rateText(m Metric, numerator, denominator int) string {
	if !m.Known {
		return fmt.Sprintf("unknown (%d reviewed)", denominator)
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*m.Value, numerator, denominator)
}

func treeLines(r *TreeReport) []string {
	if r == nil {
		return []string{"Session tree not loaded; reopen /analyze to observe descendants."}
	}
	out := []string{"Session tree observed " + timestamp(r.ObservedAt), "Requests = canonical responses + separately recorded compactions (provider-internal attempts may be unreported).", "TPS uses only output paired with generation timing; client_clock is derived.", "Wall time sums child intervals; first response means first completed response.", "List price is a derived equivalent, not billed subscription cost. " + r.PriceTable}
	if r.Partial {
		out = append(out, "partial Session tree; unobserved sessions have unknown volume/timing")
	}
	for _, g := range r.Layers {
		requests := fmt.Sprint(g.Requests)
		if g.Unobserved > 0 {
			requests += fmt.Sprintf(" observed; %d sessions unknown", g.Unobserved)
		}
		out = append(out, "", g.Layer+" / "+requestModelLabel(g.ModelKey, g.Fast), fmt.Sprintf("  layer: %d recorded / %d derived; sessions %d", g.RecordedLayers, g.DerivedLayers, g.Sessions), "  requests "+requests+"; input "+value(g.Usage.Input, g.Usage, llm.UsageInput)+"; output "+value(g.Usage.Output, g.Usage, llm.UsageOutput)+"; cached reads "+value(g.Usage.CachedInput, g.Usage, llm.UsageCachedInput), "  output TPS "+metricText(g.OutputTPS, " tok/s")+"; request mean "+metricText(g.RequestDuration, "s"), "  child wall total "+metricText(g.ChildWallTime, "s")+"; first response total "+metricText(g.FirstResponse, "s"), "  usage-limit wait "+metricText(g.UsageLimitWait, "s")+"; list price "+metricText(g.ListPriceUSD, " USD"))
	}
	out = append(out, "", "Worker rework: denominator = normally ended workers explicitly accepted/rejected.", "Repeated same-parent titles after an end infer redelegation; this does not create a review.", "Task totals are derived chains, attributed to the initial worker model, including every retry's worker time/tokens.", "Task elapsed includes parent gaps; parent tokens cannot be assigned to tasks and remain in layer totals.")
	for _, w := range r.Workers {
		x := w.Rework
		out = append(out, modelLabel(w.ModelKey), fmt.Sprintf("  normal %d; reviewed %d (accept %d / reject %d); unreviewed %d; failed %d; canceled %d; end unknown %d", x.Normal, x.Reviewed, x.Accepted, x.Rejected, x.Unreviewed, x.Failed, x.Canceled, x.UnknownEnd), "  explicit reject rate (measured) "+rateText(x.RejectRate, x.Rejected, x.Reviewed), "  rework share (explicit + derived redelegation) "+rateText(x.ReworkRate, x.Reworked, x.Reviewed)+fmt.Sprintf("; redelegated %d (derived); rejection events %d (measured)", x.Redelegated, x.RejectionEvents), fmt.Sprintf("  tasks %d (derived; mixed-model %d); attempts %d", w.Tasks, w.MixedModelTasks, w.Attempts), "  worker time incl. rework "+metricText(w.WallTime, "s")+"; per task "+metricText(w.TimePerTask, "s"), "  total tokens incl. rework "+metricText(w.TotalTokens, "")+"; per task "+metricText(w.TokensPerTask, ""))
		out = append(out, "  task elapsed incl. parent gaps "+metricText(w.Elapsed, "s")+"; per task "+metricText(w.ElapsedPerTask, "s"))
	}
	for _, task := range r.Tasks {
		out = append(out, fmt.Sprintf("  %s / %s: attempts %d; worker time %s; elapsed incl. parent gaps %s; tokens %s (task grouping derived)", task.ID, modelLabel(task.ModelKey), task.Attempts, metricText(task.WallTime, "s"), metricText(task.Elapsed, "s"), metricText(task.TotalTokens, "")))
	}
	out = append(out, "", "Per-session observations (recorded start or first response; measured, derived or unknown)")
	for _, s := range r.Sessions {
		out = append(out, fmt.Sprintf("%s [%s; layer %s; selection %s] %s", s.ID, s.Layer, s.LayerSource, s.SelectionSource, modelLabel(s.ModelKey)), "  wall "+metricText(s.WallTime, "s")+"; first response "+metricText(s.FirstResponse, "s")+"; wait "+metricText(s.UsageLimitWait, "s"))
		if !s.UsageRetryAt.IsZero() {
			out = append(out, "  "+llm.UsageLimitWaiting(s.UsageRetryAt))
		}
		for i, req := range s.Requests {
			out = append(out, fmt.Sprintf("  request %d (%s) %s: duration %s; output TPS %s", i+1, req.Kind, requestModelLabel(req.ModelKey, req.Fast), metricText(req.Duration, "s"), metricText(req.OutputTPS, " tok/s")))
		}
	}
	for i := range out {
		out[i] = strings.TrimRight(out[i], " ")
	}
	return out
}
