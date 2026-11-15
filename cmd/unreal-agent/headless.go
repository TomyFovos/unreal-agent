package main

import (
	"context"
	"encoding/json/v2"
	"net"
	"os"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

// executeHeadless uses the same serving Host, runtime, gateway and child CLI as
// the TUI. Only the attachment is replaced by one input and a completion wait.
func executeHeadless(ctx context.Context, socket string, config gateway.Config, id session.ID, promptFile string, mode permission.Mode) error {
	prompt, err := os.ReadFile(promptFile)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(string(prompt))
	if err != nil {
		return err
	}
	serving, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- gateway.ListenAndServe(serving, socket, config) }()
	defer func() { cancel(); <-done }()
	// Wait for the actual listener, including startup failure, before any model
	// can call the child CLI. The task's caller owns cancellation/timeouts.
	for {
		connection, e := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if e == nil {
			connection.Close()
			break
		}
		select {
		case e = <-done:
			done <- e
			return e
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	stop, err := json.Marshal(inbox.ControlMessage{Mode: inbox.StopWhenIdle})
	if err != nil {
		return err
	}
	current, err := config.Host.Open(ctx, host.Options{Mode: host.Create, ID: id, Lifecycle: "one-shot", Configuration: config.Configuration, Policy: config.Policy, PermissionMode: mode, Workspace: config.Workspace, Initial: []inbox.Input{{ID: "task", Kind: inbox.InputExternal, Payload: payload}, {ID: "task-complete", Kind: inbox.InputControl, Payload: stop}}})
	if err != nil {
		return err
	}
	return current.Wait(ctx)
}
