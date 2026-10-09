package authcli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/openaicodex"
)

// Non-inference preflight, deliberately separate from managed credential login.
// It reads only the specified kit files; it creates no store or Session.
func checkDockerSandboxOAuth(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("check-docker-sandbox-oauth", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("codex-auth-file", "", "existing proxy-managed auth.json path")
	forward := flags.Bool("check-forward-proxy", false, "also validate runtime-owned Docker forward proxy; no network probe")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *path == "" {
		return &credential.Error{Code: "invalid_arguments"}
	}
	if (openaicodex.DockerSandboxSource{AuthFile: *path}).Validate() != nil {
		return &credential.Error{Code: "docker_sandbox_oauth_unavailable"}
	}
	if *forward {
		if _, err := openaicodex.DockerSandboxProxyURL(nil); err != nil {
			return &credential.Error{Code: "docker_sandbox_proxy_unavailable"}
		}
	}
	_, err := fmt.Fprintln(output, "docker_sandbox_oauth: ready; host-owned ChatGPT OAuth; no imported token/account; connectivity not probed")
	return err
}
