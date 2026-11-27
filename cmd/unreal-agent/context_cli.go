package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextstate"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/record"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

const contextUsage = `usage: unreal-agent state show
       unreal-agent state decide TEXT [--id ID] [--why TEXT] [--basis LOCATOR] [--rejected JSON]
                                 [--plan ID] [--child HANDLE] [--request ID]
       unreal-agent state plan ID [--text TEXT] [--status todo|doing|done|dropped]
                                  [--position N] [--owner self|HANDLE] [--depends-on ID] [--decision ID]
       unreal-agent state rule add EXCERPT --source LOCATOR [--id ID]
       unreal-agent state accept|reject HANDLE [--why TEXT]
       unreal-agent state question [TEXT] [--id ID] [--source LOCATOR]
       unreal-agent state answer ID --source LOCATOR
       unreal-agent state import SESSION --sessions DIR
       unreal-agent record show LOCATOR [--page-bytes N] [--json]
       unreal-agent record find [TEXT] [--kind KIND] [--handle HANDLE] [--from RFC3339] [--before RFC3339]

Pass - as a text argument to read it verbatim from stdin.`

func runContextCLI(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	var cmd contextstate.Command
	var asJSON bool
	var err error
	if len(args) >= 2 && args[0] == "state" && args[1] == "import" {
		cmd, err = parseImportCommand(ctx, args[2:], out)
	} else {
		cmd, asJSON, err = parseContextCommand(args, stdin, out)
	}
	if err != nil {
		return err
	}
	socket, id := os.Getenv(subagentSocketEnv), session.ID(os.Getenv(subagentSessionEnv))
	if socket == "" || id == "" {
		return errors.New("unreal-agent state and record require the current Unreal Agent Session's shell")
	}
	client := gateway.NewClient(socket)
	defer client.Close()
	var generation string
	switch cmd.Action {
	case contextstate.Show, contextstate.ShowRecord, contextstate.FindRecord:
	default:
		current, err := client.Version(ctx, id)
		if err != nil {
			return err
		}
		generation = current.Generation
	}
	result, err := client.ContextCommand(ctx, id, generation, uuid.New().String(), cmd)
	if err != nil {
		return err
	}
	if result.Page != nil && !asJSON {
		_, err = out.Write(result.Page.Bytes)
		return err
	}
	encoded, err := result.JSON(cmd.Action, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(encoded))
	return err
}

// parseImportCommand replays another Session's history read-only and sends
// its current rules, decisions, open plan steps and open questions.
func parseImportCommand(ctx context.Context, args []string, out io.Writer) (contextstate.Command, error) {
	var cmd contextstate.Command
	flags := flag.NewFlagSet("unreal-agent state import", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() { fmt.Fprintln(out, contextUsage) }
	dir := flags.String("sessions", "", "the other Session's session directory")
	rest, err := parseAll(flags, args)
	if err != nil {
		return cmd, err
	}
	if len(rest) != 1 || *dir == "" {
		return cmd, errors.New(contextUsage)
	}
	store, err := localfile.OpenExisting(*dir)
	if err != nil {
		return cmd, err
	}
	state, err := store.ContextState(ctx, session.ID(rest[0]))
	if err != nil {
		return cmd, err
	}
	cmd.Action = contextstate.Import
	cmd.Args, err = json.Marshal(contextstate.ImportFrom(rest[0], state))
	return cmd, err
}

func parseContextCommand(args []string, stdin io.Reader, out io.Writer) (contextstate.Command, bool, error) {
	var cmd contextstate.Command
	usage := errors.New(contextUsage)
	if len(args) < 2 {
		return cmd, false, usage
	}
	action, args := args[0]+" "+args[1], args[2:]
	if len(args) > 0 && (action == "state rule" && args[0] == "add" || action == "state question" && args[0] == "answer") {
		action, args = action+" "+args[0], args[1:]
	}
	flags := flag.NewFlagSet("unreal-agent "+action, flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() { fmt.Fprintln(out, contextUsage) }
	var asJSON bool
	build := func(op contextstate.Action, payload any, position func([]string) error) error {
		rest, err := parseAll(flags, args)
		if err != nil {
			return err
		}
		if err = position(rest); err != nil {
			return err
		}
		cmd.Action = op
		cmd.Args, err = json.Marshal(payload)
		return err
	}
	noArgs := func(rest []string) error {
		if len(rest) != 0 {
			return usage
		}
		return nil
	}
	one := func(dst *string, optional bool) func([]string) error {
		return func(rest []string) error {
			if len(rest) == 0 && optional {
				return nil
			}
			if len(rest) != 1 {
				return usage
			}
			*dst = rest[0]
			return nil
		}
	}
	text := func(dst *string) error {
		if *dst == "-" {
			raw, err := io.ReadAll(stdin)
			if err != nil {
				return err
			}
			*dst = string(raw)
		}
		return nil
	}
	locator := func(dst *record.Locator, name string) {
		flags.Func(name, "canonical record locator", func(value string) error { *dst = record.Locator(value); return nil })
	}
	list := func(dst *[]string, name string) {
		flags.Func(name, "related ID, repeatable", func(v string) error { *dst = append(*dst, v); return nil })
	}
	switch action {
	case "state show":
		err := build(contextstate.Show, struct{}{}, noArgs)
		return cmd, asJSON, err
	case "state decide":
		var d contextstate.DecisionNote
		flags.StringVar(&d.ID, "id", "", "decision ID")
		flags.StringVar(&d.Why, "why", "", "reason for the decision")
		flags.Func("basis", "evidence locator, repeatable", func(v string) error { d.Basis = append(d.Basis, record.Locator(v)); return nil })
		flags.Func("rejected", "JSON array of alternatives and reasons", func(v string) error { return json.Unmarshal([]byte(v), &d.Rejected, json.RejectUnknownMembers(true)) })
		list(&d.Plan, "plan")
		list(&d.Children, "child")
		list(&d.Requests, "request")
		err := build(contextstate.Decide, &d, func(rest []string) error {
			if err := one(&d.Decision, false)(rest); err != nil {
				return err
			}
			return text(&d.Decision)
		})
		return cmd, asJSON, err
	case "state plan":
		var p contextstate.PlanNote
		flags.Func("text", "step text", func(v string) error { p.Text = &v; return text(p.Text) })
		flags.Func("status", "step status", func(v string) error { p.Status = contextstate.PlanStatus(v); return nil })
		flags.Func("position", "step order", func(v string) error {
			n, err := strconv.Atoi(v)
			p.Position = &n
			return err
		})
		flags.Func("owner", "step owner", func(v string) error { p.Owner = &v; return nil })
		list(&p.DependsOn, "depends-on")
		list(&p.Decisions, "decision")
		err := build(contextstate.SetPlan, &p, one(&p.ID, false))
		return cmd, asJSON, err
	case "state rule add":
		var r contextstate.RuleNote
		flags.StringVar(&r.ID, "id", "", "rule ID")
		locator(&r.Source, "source")
		err := build(contextstate.AddRule, &r, func(rest []string) error {
			if err := one(&r.Excerpt, false)(rest); err != nil {
				return err
			}
			return text(&r.Excerpt)
		})
		return cmd, asJSON, err
	case "state accept", "state reject":
		op := contextstate.Accept
		if action == "state reject" {
			op = contextstate.Reject
		}
		var r contextstate.ReviewNote
		flags.StringVar(&r.Why, "why", "", "optional review reason")
		err := build(op, &r, func(rest []string) error {
			if err := one(&r.Handle, false)(rest); err != nil {
				return err
			}
			return text(&r.Why)
		})
		return cmd, asJSON, err
	case "state question":
		var q contextstate.QuestionNote
		flags.StringVar(&q.ID, "id", "", "question ID")
		locator(&q.Source, "source")
		err := build(contextstate.AskUser, &q, func(rest []string) error {
			if err := one(&q.Text, q.Source != "")(rest); err != nil {
				return err
			}
			return text(&q.Text)
		})
		return cmd, asJSON, err
	case "state answer", "state question answer":
		var q contextstate.AnswerNote
		locator(&q.Source, "source")
		err := build(contextstate.AnswerUser, &q, one(&q.ID, false))
		return cmd, asJSON, err
	case "record show":
		var r contextstate.RecordSelection
		flags.Int64Var(&r.PageBytes, "page-bytes", 0, "bytes per page, zero for the full selection")
		flags.BoolVar(&asJSON, "json", false, "return text or base64 in JSON")
		err := build(contextstate.ShowRecord, &r, func(rest []string) error {
			if len(rest) != 1 {
				return usage
			}
			r.Locator = record.Locator(rest[0])
			return nil
		})
		return cmd, asJSON || r.PageBytes != 0, err
	case "record find":
		var r contextstate.RecordSearch
		flags.Func("kind", "record kind", func(v string) error { r.Kind = record.Kind(v); return nil })
		flags.StringVar(&r.Handle, "handle", "", "child handle")
		timestamp := func(dst *time.Time, name string) {
			flags.Func(name, "RFC3339 timestamp", func(v string) error {
				var err error
				*dst, err = time.Parse(time.RFC3339, v)
				return err
			})
		}
		timestamp(&r.From, "from")
		timestamp(&r.Before, "before")
		err := build(contextstate.FindRecord, &r, one(&r.Text, true))
		return cmd, asJSON, err
	default:
		return cmd, asJSON, usage
	}
}
