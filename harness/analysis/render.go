package analysis

import (
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"slices"
	"strings"
)

var Views = []string{"Overview", "Usage", "Turns", "Tools", "Agents", "Context", "Timeline", "Errors", "Export"}

func value(v int64, u viewer.Usage, field ...llm.UsageField) string {
	if !u.Known || len(field) > 0 && slices.Contains(u.Unknown, field[0]) && v == 0 {
		return "unknown"
	}
	prefix := ""
	if u.Partial {
		prefix = "~"
	}
	return prefix + fmt.Sprint(v)
}
func Lines(r Report, view string, ascii bool) []string {
	var out []string
	if r.Partial {
		out = append(out, "partial/incomplete history")
	}
	if r.Resync {
		out = append(out, "resync: observed canonical data only")
	}
	switch strings.ToLower(view) {
	case "overview":
		out = append(out, "Session     "+string(r.Session))
		if r.Selection != nil {
			out = append(out, "Provider    "+r.Selection.Provider, "Model       "+r.Selection.Name, "Effort      "+r.Selection.Effort)
			out = append(out, "Selection   "+r.Selection.Model)
			out = append(out, fmt.Sprintf("Runtime revision  %d", r.Selection.Revision), "Provider capability  "+r.ToolCapability)
		} else {
			out = append(out, "Model/effort unknown")
		}
		if r.ManagedPolicyMode != "" {
			mode := "blocked (text-only)"
			if r.ToolBridgeEnabled {
				mode = "blocked; Unreal SDK MCP bridge enabled"
				if r.ToolBridgeMode == "structured" {
					mode = "side-effecting tools blocked; StructuredOutput serializer only"
				}
			}
			out = append(out, "Managed policy mode  "+r.ManagedPolicyMode, "Tool ownership       Unreal Agent", "Claude tools         "+mode)
			if r.ManagedPolicyMode == "trust" {
				out = append(out, "Trusted organization policy: managed behavior may run outside Unreal Operations.")
			}
		}
		if r.ToolBridgeMode != "" {
			out = append(out, "Bridge mode          "+r.ToolBridgeMode)
		}
		if r.ToolBridgeStatus != "" {
			out = append(out, "Tool Bridge          "+r.ToolBridgeStatus+" (live registration)")
		}
		out = append(out, fmt.Sprintf("Responses   %d", r.Usage.Responses))
		elapsed := "unknown"
		if r.Elapsed.Known {
			elapsed = r.Elapsed.Value.Round(1e9).String()
		}
		out = append(out, "Elapsed     "+elapsed, "Tokens")
		out = append(out, usageLines(r.Usage)...)
		out = append(out, fmt.Sprintf("Tools       %d calls · %d completed · %d failed · %d canceled", r.Tools.Calls, r.Tools.Completed, r.Tools.Failed, r.Tools.Canceled), fmt.Sprintf("Agents      %d discovered · %d observed", r.AgentCounts.Discovered, r.AgentCounts.Observed), fmt.Sprintf("            %d success · %d failed · %d canceled · %d finish unknown", r.AgentCounts.Success, r.AgentCounts.Failed, r.AgentCounts.Canceled, r.AgentCounts.UnknownFinish))
	case "usage":
		out = append(out, usageLines(r.Usage)...)
	case "turns":
		out = append(out, "TURN PROVIDER / MODEL / EFFORT · INPUT CACHED REASON OUTPUT")
		for _, t := range r.Turns {
			model := "unknown"
			if t.Selection != nil {
				model = t.Selection.Provider + " / " + t.Selection.Name + " / " + t.Selection.Effort
				if t.ObservedModel != "" {
					model += " (observed " + t.ObservedModel + ")"
				}
			}
			out = append(out, fmt.Sprintf("%d %s · %s %s %s %s", t.Number, model, value(t.Usage.Input, t.Usage, llm.UsageInput), value(t.Usage.CachedInput, t.Usage, llm.UsageCachedInput), value(t.Usage.Reasoning, t.Usage, llm.UsageReasoning), value(t.Usage.Output, t.Usage, llm.UsageOutput)))
		}
		if len(r.Turns) == 0 {
			out = append(out, "no observed turns")
		}
	case "tools":
		out = append(out, fmt.Sprintf("Calls       %d", r.Tools.Calls), fmt.Sprintf("Completed   %d", r.Tools.Completed), fmt.Sprintf("Failed      %d", r.Tools.Failed), fmt.Sprintf("Canceled    %d", r.Tools.Canceled), fmt.Sprintf("Unresolved  %d", r.Tools.Unresolved))
		for _, tool := range r.ByTool {
			elapsed := "unknown"
			if tool.Elapsed.Known {
				elapsed = tool.Elapsed.Value.Round(1e9).String()
				if tool.ElapsedPartial {
					elapsed = "~" + elapsed
				}
			}
			out = append(out, fmt.Sprintf("%s: %d calls / %d done / %d failed / %d canceled; elapsed %s", tool.Name, tool.Calls, tool.Completed, tool.Failed, tool.Canceled, elapsed))
		}
	case "agents":
		if len(r.Agents) == 0 {
			out = append(out, "no discovered child agents")
		}
		for _, a := range r.Agents {
			out = append(out, string(a.ID)+" · "+string(a.Runtime)+" · parent "+string(a.ParentStatus))
			if a.Selection != nil {
				out = append(out, "  runtime     "+a.Selection.Provider+" / "+a.Selection.Model+" / "+a.Selection.Effort)
			}
			if a.Finish != "" {
				out = append(out, "  finish      "+a.Finish)
			} else {
				out = append(out, "  finish      unknown")
			}
			if !a.Observed {
				out = append(out, "  unobserved/partial; usage and operations unknown")
				continue
			}
			elapsed := "unknown"
			if a.Elapsed.Known {
				elapsed = a.Elapsed.Value.Round(1e9).String()
			}
			out = append(out, "  elapsed     "+elapsed, "  usage       in "+value(a.Usage.Input, a.Usage, llm.UsageInput)+" / out "+value(a.Usage.Output, a.Usage, llm.UsageOutput))
			if a.Operations != nil {
				out = append(out, fmt.Sprintf("  operations  %d", *a.Operations))
			}
		}
	case "context":
		if d := r.ContextPackage; d != nil {
			if d.TransportReserve > 0 {
				out = append(out, fmt.Sprintf("Transport / current receipt reserve  %d (estimated)", d.TransportReserve))
			}
			out = append(out, "Context Engine v1 · estimated (not actual usage)", "Provider / model  "+viewer.SafeText(d.Provider)+" / "+viewer.SafeText(d.Model), fmt.Sprintf("Runtime revision  %d", d.RuntimeRevision))
			out = append(out, fmt.Sprintf("Canonical context units  %d", d.CanonicalUnits), fmt.Sprintf("Recent raw selected      %d", d.Recent), fmt.Sprintf("Retrieved                %d", d.Retrieved), fmt.Sprintf("Referenced / omitted     %d / %d", d.Referenced, d.Omitted), fmt.Sprintf("Security exclusions      %d", d.Excluded))
			out = append(out, fmt.Sprintf("Retrieval candidates / selected  %d / %d", d.Retrieval.Candidates, d.Retrieval.Selected), fmt.Sprintf("Skipped recent/duplicate/budget/ineligible  %d / %d / %d / %d", d.Retrieval.SkippedRecent, d.Retrieval.SkippedDuplicate, d.Retrieval.SkippedBudget, d.Retrieval.SkippedIneligible))
			out = append(out, fmt.Sprintf("Checkpoint               v%d through sequence %d", d.CheckpointVersion, d.CheckpointBoundary), fmt.Sprintf("Request source boundary  %d", d.ThroughSequence), fmt.Sprintf("Estimated input tokens   %d / %d · %.1f%%", d.EstimatedInputTokens, d.Budget.Input, d.Utilization), fmt.Sprintf("Window                   %d (%s)", d.Budget.Window, d.Budget.WindowSource), fmt.Sprintf("Reserves response/schema/protocol  %d / %d / %d", d.Budget.ResponseReserve, d.Budget.SchemaReserve, d.Budget.ProtocolReserve), fmt.Sprintf("Retrieval latency        %d us", d.RetrievalMicros), "Derived cache            "+d.Cache)
		} else {
			out = append(out, "Context package unknown (no prepared request observed)")
		}
		if len(r.Context) == 0 {
			out = append(out, "input usage unknown")
			break
		}
		latest := r.Context[len(r.Context)-1]
		out = append(out, "Latest known input  "+value(latest.Usage.Input, latest.Usage, llm.UsageInput)+fmt.Sprintf(" (turn %d)", latest.Number))
		if !r.Partial && len(r.Turns) > 0 && latest.Number == r.Turns[len(r.Turns)-1].Number && !latest.Usage.Partial && latest.Selection != nil && latest.Selection.ContextWindow > 0 {
			out = append(out, fmt.Sprintf("%d / %d · %.1f%%", latest.Usage.Input, latest.Selection.ContextWindow, 100*float64(latest.Usage.Input)/float64(latest.Selection.ContextWindow)))
		}
		maximum := int64(1)
		for _, t := range r.Context {
			maximum = max(maximum, t.Usage.Input)
		}
		mark := "█"
		if ascii {
			mark = "#"
		}
		for _, t := range r.Context {
			n := int(24 * float64(t.Usage.Input) / float64(maximum))
			out = append(out, fmt.Sprintf("Turn %-4d %9s %s", t.Number, value(t.Usage.Input, t.Usage, llm.UsageInput), strings.Repeat(mark, max(0, min(24, n)))))
		}
	case "timeline":
		if len(r.Timeline) == 0 {
			out = append(out, "no canonical timestamps")
		}
		for _, e := range r.Timeline {
			out = append(out, e.At.Format("15:04:05")+" "+e.Kind+" "+e.Name)
		}
	case "errors":
		crashes := fmt.Sprint(r.Errors.Crashes)
		if !r.Errors.CrashesKnown {
			crashes = "~" + crashes + " observed (partial history)"
		}
		out = append(out, fmt.Sprintf("Provider    %d canonical records", r.Errors.Provider), fmt.Sprintf("Tools       %d", r.Errors.Tools), fmt.Sprintf("Children    %d", r.Errors.Children), "Crashes     "+crashes, "recent (metadata only)")
		for _, e := range r.Errors.Recent {
			at := "time unknown"
			if !e.At.IsZero() {
				at = e.At.Format("15:04:05")
			}
			out = append(out, at+" "+e.Kind+" "+e.Name)
		}
		out = append(out, "Nonpersisted Host/transport failures are outside canonical history.")
	default:
		out = append(out, "analysis unavailable")
	}
	if ascii {
		for i, s := range out {
			out[i] = strings.ReplaceAll(s, " · ", " / ")
		}
	}
	for i, s := range out {
		out[i] = viewer.SafeText(s)
	}
	return out
}
func usageLines(u viewer.Usage) []string {
	out := []string{fmt.Sprintf("responses     %d", u.Responses), "input         " + value(u.Input, u, llm.UsageInput), "cached input  " + value(u.CachedInput, u, llm.UsageCachedInput), "cache write   " + value(u.CacheWriteInput, u, llm.UsageCacheWriteInput), "reasoning     " + value(u.Reasoning, u, llm.UsageReasoning), "output        " + value(u.Output, u, llm.UsageOutput)}
	if u.Partial {
		out = append(out, "partial")
	}
	if !u.Known {
		out = append(out, "usage unknown")
	}
	return out
}
