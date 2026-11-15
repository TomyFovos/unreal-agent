// Command unreal-agent-runner executes one JSON request and writes persisted session items as JSONL.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
)

// Set by builds without VCS metadata (for example the Docker builder).
var version string

func revision() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "dev"
}

func main() {
	os.Exit(run(os.Stdin))
}

func run(input io.Reader) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "child" {
		if err := agentrunner.RunChild(ctx, os.Args[2:], os.Stdin, os.Stdout, os.Stderr, revision()); err != nil {
			return 1
		}
		return 0
	}
	return agentrunner.RunMain(
		ctx, os.Args[1:], os.Getenv, os.Environ,
		input, os.Stdout, os.Stderr,
		agentrunner.Config{
			Name:         "unreal-agent-runner",
			Version:      revision(),
			ParseRequest: parseRequest,
			Providers:    agentrunner.DefaultProviders(),
		},
	)
}
