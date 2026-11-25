package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/usage"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
)

func runUsageCLI(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("unreal-agent usage", flag.ContinueOnError)
	flags.SetOutput(out)
	since := flags.Duration("since", time.Hour, "duration of the additional recent window")
	asJSON := flags.Bool("json", false, "write the full ledger as JSON")
	directory := flags.String("session-directory", "", "read this canonical session directory without a Host")
	id := flags.String("session", "", "Session ID for an offline read")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *since <= 0 || (*directory == "") != (*id == "") {
		return errors.New("usage: unreal-agent usage [--since 60m] [--json] [--session-directory PATH --session ID]")
	}
	var report subagent.UsageReport
	var err error
	if *directory != "" {
		reader, e := subagent.OpenUsageReader(*directory)
		if e != nil {
			return e
		}
		report, err = reader.Read(ctx, session.ID(*id), time.Now(), *since)
	} else {
		socket, root := os.Getenv(subagentSocketEnv), session.ID(os.Getenv(subagentSessionEnv))
		if socket == "" || root == "" {
			return errors.New("usage needs an Unreal Agent Session shell or --session-directory and --session")
		}
		client := gateway.NewClient(socket)
		defer client.Close()
		report, err = (viewer.Remote{ParentID: root, Call: client.Extension}).UsageTree(ctx, *since)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	renderUsageCLI(out, report)
	return nil
}

func renderUsageCLI(out io.Writer, r subagent.UsageReport) {
	fmt.Fprintf(out, "Session tree %s\n", usage.Label(string(r.Root)))
	if r.Partial {
		fmt.Fprintln(out, "Partial ledger: some Session records are unavailable.")
	}
	for _, window := range []struct {
		label  string
		totals usage.Totals
	}{
		{"Caller only", r.Own.All.Totals}, {"Tree all time", r.All.Totals}, {"10m", r.Last10m.Totals}, {"60m", r.Last60m.Totals}, {"Selected " + r.Since, r.Recent.Totals},
	} {
		fmt.Fprintf(out, "%s · %d calls · input %s · output %s · charge %s\n", window.label, window.totals.Calls,
			usageCount(window.totals.Input), usageCount(window.totals.Output), window.totals.DollarText())
	}
	fmt.Fprintf(out, "Selected window rate · %.1f calls/h\n", r.CallsPerHour)
	for _, line := range usage.ProviderLines(r.All.ByProvider, r.Allowances) {
		fmt.Fprintln(out, line)
	}
	for _, spend := range r.Spend {
		if spend.Hourly != nil {
			rate := usage.Money(spend.Currency, *spend.Hourly, 2) + "/h"
			if spend.RatePartial {
				rate += " (partial)"
			}
			fmt.Fprintf(out, "%s · EMA %s\n", usage.Label(spend.Provider), rate)
		}
	}
	for _, section := range []struct {
		title string
		rows  []usage.Group
	}{{"Provider / model", r.All.ByModel}, {"Role", r.All.ByRole}} {
		fmt.Fprintln(out, "\n"+section.title)
		for _, g := range section.rows {
			label := usage.Label(g.Role)
			if g.Role == "" {
				label = usage.Label(g.Provider) + " / " + usage.Label(g.Model)
			}
			fmt.Fprintf(out, "%s · %d calls · charge %s · input %s (uncached %s, cache read %s, cache write %s) · output %s · reasoning %s\n",
				label, g.Calls, g.DollarText(), usageCount(g.Input), usageCount(g.Uncached), usageCount(g.CacheRead),
				usageCount(g.CacheWrite), usageCount(g.Output), usageCount(g.Reasoning))
		}
	}
	for _, provider := range r.All.ByProvider {
		for _, amount := range provider.Charges {
			if amount.BillingSource == "" {
				continue
			}
			fmt.Fprintf(out, "%s %s · %d calls · charged %s · list %s\n", usage.Label(provider.Provider), usage.Label(amount.BillingSource),
				amount.Calls, amountText(amount.Currency, amount.Charged), amountText(amount.Currency, amount.List))
		}
	}
	if len(r.TopChildren) > 0 {
		fmt.Fprintln(out, "\nTop children")
		for _, c := range r.TopChildren[:min(10, len(r.TopChildren))] {
			fmt.Fprintf(out, "%s · %s · %s · %s\n", c.Handle, shortUsageTitle(c.Title), c.State, c.UsageText())
		}
	}
}

func shortUsageTitle(s string) string {
	runes := []rune(strings.Join(strings.Fields(s), " "))
	if len(runes) > 80 {
		return string(runes[:77]) + "..."
	}
	return string(runes)
}

func usageCount(c usage.Counter[int64]) string {
	if c.Value == nil {
		return "unknown"
	}
	if c.Unknown > 0 {
		return fmt.Sprintf("%d (partial)", *c.Value)
	}
	return fmt.Sprint(*c.Value)
}

func amountText(currency string, c usage.Counter[float64]) string {
	if c.Value == nil {
		return "unknown"
	}
	text := usage.Money(currency, *c.Value, 2)
	if c.Unknown > 0 {
		text += " (partial)"
	}
	return text
}
