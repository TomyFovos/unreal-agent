package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

type Client interface {
	Reader
	Open(context.Context, host.Mode, session.ID) (host.View, error)
	Submit(context.Context, session.ID, string, inbox.Input) (host.Receipt, error)
	Methods(context.Context) ([]authflow.Support, error)
	Login(context.Context, credential.Reference, credential.Secret) (credential.Metadata, error)
	Logout(context.Context, credential.Reference) error
	ListCredentials(context.Context) ([]credential.Metadata, error)
}
type Config struct {
	Client Client
	ID     session.ID
	Keys   <-chan Key
	Output io.Writer
	Size   func() (int, int)
	// Panel and Command connect optional child viewers through their parent-owned
	// controller. No Session store or model calls are exposed to the UI.
	Panel   func(width, height int) string
	Updates <-chan struct{}
	Command func(context.Context, string) (string, bool)
}
type commandResult struct {
	text  string
	err   error
	input *inbox.Input
}

func Run(ctx context.Context, c Config) error {
	if c.Client == nil || c.Output == nil || c.Keys == nil {
		return errors.New("tui: client, output and keys required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := NewModel(c.ID)
	changes := make(chan struct{}, 1)
	watchDone := make(chan struct{})
	notify := func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	}
	go func() { defer close(watchDone); _ = Watch(ctx, c.Client, m, notify) }()
	defer func() { cancel(); <-watchDone }()
	var editor Editor
	defer editor.Clear()
	var private *credential.Reference
	var retry *inbox.Input
	results := make(chan commandResult, 1)
	busy := false
	var cancelJob context.CancelFunc
	start := func(fn func(context.Context) commandResult) {
		busy = true
		jobCtx, stop := context.WithCancel(ctx)
		cancelJob = stop
		go func() {
			defer stop()
			r := fn(jobCtx)
			select {
			case results <- r:
			case <-ctx.Done():
			}
		}()
	}
	send := func(input inbox.Input) {
		generation := m.Snapshot().Generation
		start(func(ctx context.Context) commandResult {
			_, err := c.Client.Submit(ctx, c.ID, generation, input)
			return commandResult{text: "input committed", err: err, input: &input}
		})
	}
	status := "/help for commands; closing this screen leaves the owner running"
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	dirty := true
	width, height := 80, 24
	for {
		if c.Size != nil {
			w, h := c.Size()
			if w != width || h != height {
				width, height = w, h
				dirty = true
			}
		}
		if dirty {
			panel := ""
			if c.Panel != nil {
				panel = c.Panel(width, height)
			}
			if _, err := io.WriteString(c.Output, Render(m.Snapshot(), editor.Display(private != nil), status, width, height, panel)); err != nil {
				return err
			}
			dirty = false
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		case <-changes:
			dirty = true
		case _, ok := <-c.Updates:
			if !ok {
				c.Updates = nil
			}
			dirty = true
		case result := <-results:
			busy = false
			cancelJob = nil
			status = result.text
			if result.err != nil {
				status = "request failed: " + result.err.Error()
				if result.input != nil {
					retry = result.input
					status += "; /retry uses the same input ID"
				}
			} else if result.input != nil {
				retry = nil
			}
			dirty = true
		case key, ok := <-c.Keys:
			if !ok || key.Name == "detach" {
				return nil
			}
			dirty = true
			if key.Name == "cancel" {
				if private != nil {
					editor.Clear()
					private = nil
					status = "login canceled"
					continue
				}
				if busy {
					if cancelJob != nil {
						cancelJob()
					}
					status = "request canceled; outcome may require refresh"
					continue
				}
				editor.Clear()
				payload, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.StopHard, Reason: "terminal user requested stop"})
				send(inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload})
				continue
			}
			if key.Name != "enter" {
				editor.Apply(key)
				continue
			}
			if busy {
				status = "request pending; Ctrl-C cancels waiting"
				continue
			}
			line := editor.Text()
			pasted := editor.Pasted
			editor.Clear()
			if private != nil {
				ref := *private
				private = nil
				secret := credential.NewSecret(line)
				line = ""
				start(func(ctx context.Context) commandResult {
					_, err := c.Client.Login(ctx, ref, secret)
					secret = credential.Secret{}
					return commandResult{text: "API key stored; authentication is checked on the next provider request", err: err}
				})
				continue
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			if strings.HasPrefix(line, "/") && !pasted {
				parts := strings.Fields(line)
				switch parts[0] {
				case "/help":
					status = "/stop [idle], /resume, /retry, /login PROVIDER ID, /logout PROVIDER ID, /methods, /credentials, /detach"
				case "/detach":
					return nil
				case "/retry":
					if retry != nil {
						send(*retry)
					} else {
						status = "no uncertain input to retry"
					}
				case "/resume":
					start(func(ctx context.Context) commandResult {
						_, err := c.Client.Open(ctx, host.Resume, c.ID)
						return commandResult{text: "session resumed", err: err}
					})
				case "/stop":
					mode := inbox.StopHard
					if len(parts) > 1 && parts[1] == "idle" {
						mode = inbox.StopWhenIdle
					}
					payload, _ := json.Marshal(inbox.ControlMessage{Mode: mode, Reason: "terminal user requested stop"})
					send(inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload})
				case "/login":
					if len(parts) != 3 {
						status = "usage: /login PROVIDER ID (then enter key privately)"
						continue
					}
					ref := credential.Reference{Provider: parts[1], ID: parts[2], Method: credential.APIKey}
					if err := authflow.ValidateMethod(ref); err != nil {
						status = err.Error()
						continue
					}
					private = &ref
					status = "API key input is masked; Ctrl-C cancels; key is never sent to the agent"
				case "/logout":
					if len(parts) != 3 {
						status = "usage: /logout PROVIDER ID"
						continue
					}
					ref := credential.Reference{Provider: parts[1], ID: parts[2], Method: credential.APIKey}
					start(func(ctx context.Context) commandResult {
						return commandResult{text: "credential removed", err: c.Client.Logout(ctx, ref)}
					})
				case "/methods":
					start(func(ctx context.Context) commandResult {
						methods, err := c.Client.Methods(ctx)
						var entries []string
						for _, m := range methods {
							entries = append(entries, fmt.Sprintf("%s/%s: %s", m.Provider, m.Method, m.Status))
						}
						return commandResult{text: strings.Join(entries, " | "), err: err}
					})
				case "/credentials":
					start(func(ctx context.Context) commandResult {
						entries, err := c.Client.ListCredentials(ctx)
						var labels []string
						for _, e := range entries {
							labels = append(labels, e.Reference.Provider+"/"+e.Reference.ID)
						}
						return commandResult{text: "credentials: " + strings.Join(labels, ", "), err: err}
					})
				default:
					if c.Command != nil {
						start(func(ctx context.Context) commandResult {
							text, ok := c.Command(ctx, line)
							if !ok {
								text = "unknown command"
							}
							return commandResult{text: text}
						})
					} else {
						status = "unknown command; /help"
					}
				}
			} else {
				payload, _ := json.Marshal(line)
				send(inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputExternal, Payload: payload})
			}
		}
	}
}
