package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runExecutable(ctx, filepath.Base(os.Args[0]), os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
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
