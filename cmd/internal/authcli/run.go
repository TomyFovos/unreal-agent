package authcli

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"flag"
	"io"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
)

// Run never accepts a secret as a flag and never echoes input. The executable
// rejects terminal stdin for login; a TUI uses authflow directly in a masked
// input surface, not its transcript or canonical Session input.
func Run(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) == 0 {
		return &credential.Error{Code: "command_required"}
	}
	if args[0] == "methods" {
		if len(args) != 1 {
			return &credential.Error{Code: "invalid_arguments"}
		}
		return json.MarshalWrite(output, authflow.Matrix())
	}
	flags := flag.NewFlagSet("unreal-agent-auth", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("store", "", "private credential directory")
	provider := flags.String("provider", "", "explicit provider")
	id := flags.String("id", "", "credential ID")
	method := flags.String("method", "api_key", "authentication method")
	keyStdin := flags.Bool("key-stdin", false, "read API key from redirected standard input")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return &credential.Error{Code: "invalid_arguments"}
	}
	if *directory == "" {
		return &credential.Error{Code: "store_required"}
	}
	backend, err := credential.OpenLocal(*directory)
	if err != nil {
		return err
	}
	service := authflow.New(credential.NewManager(backend, nil))
	ref := credential.Reference{Provider: *provider, ID: *id, Method: credential.Method(*method)}
	switch args[0] {
	case "login":
		if !*keyStdin {
			return &credential.Error{Code: "private_stdin_required"}
		}
		if err := authflow.ValidateMethod(ref); err != nil {
			return err
		}
		raw, err := readSecret(ctx, input)
		if err != nil {
			return err
		}
		metadata, err := service.Login(ctx, authflow.LoginRequest{Reference: ref, Secret: credential.NewSecret(string(raw))})
		clear(raw)
		if err != nil {
			return err
		}
		return json.MarshalWrite(output, metadata)
	case "logout":
		if err := service.Logout(ctx, ref); err != nil {
			return err
		}
		return json.MarshalWrite(output, struct {
			Removed bool `json:"removed"`
		}{true})
	case "list":
		entries, err := service.List(ctx)
		if err != nil {
			return err
		}
		return json.MarshalWrite(output, entries)
	default:
		return &credential.Error{Code: "unsupported_command"}
	}
}

// readSecret owns input cancellation only for this private login channel.
// Blocking readers must be closeable; bounded memory readers need no interrupt.
func readSecret(ctx context.Context, input io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if closer, ok := input.(io.ReadCloser); ok {
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { _ = closer.Close(); close(closed) })
		defer func() {
			if !stop() {
				<-closed
			}
		}()
	} else {
		switch input.(type) {
		case *strings.Reader, *bytes.Reader, *bytes.Buffer:
		default:
			return nil, &credential.Error{Code: "input_not_cancelable"}
		}
	}
	raw, err := io.ReadAll(io.LimitReader(input, 16385))
	if ctx.Err() != nil {
		clear(raw)
		return nil, ctx.Err()
	}
	if err != nil || len(raw) > 16384 {
		clear(raw)
		return nil, &credential.Error{Code: "invalid_secret_input"}
	}
	return raw, nil
}
