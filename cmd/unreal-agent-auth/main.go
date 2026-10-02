// Command unreal-agent-auth manages credentials outside Session transcripts.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/unreallabsai/unreal-agent/cmd/internal/authcli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if len(os.Args) > 1 && os.Args[1] == "login" {
		info, err := os.Stdin.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprintln(os.Stderr, "credential: private_redirected_stdin_required")
			os.Exit(1)
		}
	}
	if err := authcli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
