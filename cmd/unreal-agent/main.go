package main

import (
	"context"
	"fmt"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runExecutableWithStreams(ctx, filepath.Base(os.Args[0]), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runExecutableWithStreams(ctx context.Context, name string, args []string, stdout, diagnostics io.Writer) error {
	if len(args) == 1 && args[0] == "--version" {
		_, err := fmt.Fprintln(stdout, buildfeatures.Version(""))
		return err
	}
	if isSubagentCommand(name, args) {
		return runSubagentCLI(ctx, args[1:], os.Stdin, stdout)
	}
	if len(args) > 0 && args[0] == "usage" && (name != "unreal" || os.Getenv(subagentSessionEnv) != "") {
		return runUsageCLI(ctx, args[1:], stdout)
	}
	if len(args) > 0 && slices.Contains([]string{"state", "record"}, args[0]) && (name != "unreal" || os.Getenv(subagentSessionEnv) != "") {
		return runContextCLI(ctx, args, os.Stdin, stdout)
	}
	output := diagnostics
	// The metadata-only subcommand is consumed by setup as machine JSON, and
	// folders prints its listings for the terminal.
	// Plain "unreal providers" and "unreal folders" remain positional launcher
	// session names.
	if name != "unreal" && len(args) != 0 && (args[0] == "providers" || args[0] == "folders") {
		output = stdout
	}
	return runExecutable(ctx, name, args, output)
}

func runExecutable(ctx context.Context, name string, args []string, output io.Writer) error {
	// A Host installed under the normal name still spawns the existing child
	// stdio protocol. Plain "unreal child" remains a positional session name.
	if len(args) >= 2 && args[0] == "child" && args[1] == "--stdio" {
		return run(ctx, args, output)
	}
	if name == "unreal" {
		return runLauncher(ctx, args, output)
	}
	return run(ctx, args, output)
}
