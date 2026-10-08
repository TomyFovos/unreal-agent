package main

import (
	"context"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"strings"
	"uuid"
)

func childStartCommand(client *gateway.Client, id session.ID, next func(context.Context, string) (string, bool)) func(context.Context, string) (string, bool) {
	var retry *modelRequest
	return func(ctx context.Context, line string) (string, bool) {
		if line == "/children retry-start" {
			if retry == nil {
				return "No uncertain child start to retry.", true
			}
		} else if strings.HasPrefix(line, "/children start ") {
			head, task, ok := strings.Cut(strings.TrimPrefix(line, "/children start "), " -- ")
			args := strings.Fields(head)
			if !ok || len(args) != 1 && len(args) != 4 || strings.TrimSpace(task) == "" {
				return "usage: /children start TEMPLATE [PROVIDER MODEL EFFORT] -- TASK (use - for no effort)", true
			}
			q := modelRequest{Action: "child.start", ID: id, InputID: inbox.ID(uuid.New().String()), Template: args[0], Task: task}
			if len(args) == 4 {
				effort := llm.ReasoningEffort(args[3])
				if args[3] == "-" {
					effort = ""
				}
				q.ChildRuntime = &subagent.RuntimeRequest{Provider: args[1], Model: args[2], Effort: effort}
			}
			retry = &q
		} else {
			return next(ctx, line)
		}
		v, e := client.Inspect(ctx, id, 0, 1)
		if e != nil {
			return "child start unavailable; /children retry-start reuses its input ID", true
		}
		retry.Generation = v.Generation
		out, e := modelExchange(ctx, client.Extension, *retry)
		if e != nil {
			return "child start failed: " + viewer.SafeText(e.Error()) + "; /children retry-start reuses its input ID", true
		}
		if out.Receipt == nil {
			return "child start reply incomplete; /children retry-start", true
		}
		retry = nil
		return "Child start accepted; /children shows its canonical operation and runtime.", true
	}
}
