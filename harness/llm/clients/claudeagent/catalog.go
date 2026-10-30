package claudeagent

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
)

// Account is the login Claude Code reports. Unreal never reads Claude's
// credential files.
type Account struct {
	Email            string `json:"email"`
	Organization     string `json:"organization"`
	SubscriptionType string `json:"subscriptionType"`
	APIProvider      string `json:"apiProvider"`
}

type cliModel struct {
	Value            string   `json:"value"`
	ResolvedModel    string   `json:"resolvedModel"`
	DisplayName      string   `json:"displayName"`
	Description      string   `json:"description"`
	SupportsEffort   *bool    `json:"supportsEffort"`
	Efforts          []string `json:"supportedEffortLevels"`
	AdaptiveThinking *bool    `json:"supportsAdaptiveThinking"`
	FastMode         *bool    `json:"supportsFastMode"`
}

// Discover reads the models and login from Claude Code's initialize reply. It
// starts no conversation and makes no model request.
func Discover(ctx context.Context, binary string, environ func() []string) (modelcatalog.Catalog, Account, error) {
	if environ == nil {
		environ = os.Environ
	}
	dir, err := os.MkdirTemp("", "unreal-claude-catalog-")
	if err != nil {
		return modelcatalog.Catalog{}, Account{}, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-prompt-tool", "stdio", "--no-session-persistence")
	cmd.Dir, cmd.Env = dir, environment(environ())
	isolate(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return modelcatalog.Catalog{}, Account{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return modelcatalog.Catalog{}, Account{}, err
	}
	var stderr tail
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		return modelcatalog.Catalog{}, Account{}, &Error{Code: "cli_missing", Message: err.Error()}
	}
	var finishOnce sync.Once
	finishProcess := func(drain bool) {
		finishOnce.Do(func() {
			_ = stdin.Close()
			if !drain {
				cancel()
			}
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			select {
			case <-exited:
			case <-ctx.Done():
				cancel()
				<-exited
			case <-time.After(cmd.WaitDelay):
				cancel()
				<-exited
			}
			cancel()
		})
	}
	defer finishProcess(false)
	if err = (&writer{w: stdin}).send(map[string]any{"type": "control_request", "request_id": "unreal-catalog", "request": map[string]any{"subtype": "initialize"}}); err != nil {
		finishProcess(true)
		return modelcatalog.Catalog{}, Account{}, startFailure(stderr.String(), &Error{Code: "catalog_unavailable", Message: lastLine(stderr.String())})
	}
	reader := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, readErr := readLine(reader)
		var f struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Error     string `json:"error"`
				Response  struct {
					Models  []cliModel `json:"models"`
					Account Account    `json:"account"`
				} `json:"response"`
			} `json:"response"`
		}
		if len(line) > 0 && json.Unmarshal(line, &f) == nil && f.Type == "control_response" && f.Response.RequestID == "unreal-catalog" {
			if f.Response.Subtype != "success" {
				return modelcatalog.Catalog{}, Account{}, &Error{Code: "catalog_unavailable", Message: f.Response.Error}
			}
			catalog := normalize(f.Response.Response.Models)
			return catalog, f.Response.Response.Account, nil
		}
		if readErr != nil {
			finishProcess(true)
			return modelcatalog.Catalog{}, Account{}, startFailure(stderr.String(), &Error{Code: "catalog_unavailable", Message: lastLine(stderr.String())})
		}
	}
}

// normalize keeps Claude Code's own model choices. "default" is left out: it
// names the same model as one of the listed aliases.
func normalize(models []cliModel) modelcatalog.Catalog {
	c := modelcatalog.Catalog{Source: "Claude Code initialize", Available: true, Authoritative: true, FetchedAt: time.Now()}
	seen := map[string]bool{}
	for _, info := range models {
		if info.Value == "" || info.Value == "default" || seen[info.Value] {
			continue
		}
		seen[info.Value] = true
		m := modelcatalog.Model{ID: info.Value, Name: info.DisplayName, ResolvedModel: info.ResolvedModel, Description: info.Description, SupportsEffort: info.SupportsEffort, AdaptiveThinking: info.AdaptiveThinking, FastMode: info.FastMode}
		if m.Name == "" {
			m.Name = m.ID
		}
		if info.SupportsEffort != nil && *info.SupportsEffort {
			for _, level := range info.Efforts {
				if e := llm.ReasoningEffort(level); e.Valid() && e != llm.ReasoningEffortUltra && !slices.Contains(m.Efforts, e) {
					m.Efforts = append(m.Efforts, e)
				}
			}
			if slices.Contains(m.Efforts, llm.ReasoningEffortHigh) {
				m.DefaultEffort = llm.ReasoningEffortHigh
			} else if len(m.Efforts) > 0 {
				m.DefaultEffort = m.Efforts[0]
			}
		}
		if len(m.Efforts) == 0 {
			no := false
			m.SupportsEffort = &no
		}
		c.Models = append(c.Models, m)
	}
	if len(c.Models) == 0 {
		c.Available, c.Problem = false, "Claude Code listed no models"
	}
	return c
}

// Subscription reports whether the login draws on a Claude plan rather than
// API billing (U5).
func (a Account) Subscription() bool { return a.SubscriptionType != "" }
