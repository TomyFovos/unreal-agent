package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
)

// Compared through the existing private extension API. No credential values,
// daemon metadata file, or replacement gateway protocol is introduced.
type launcherHostBinding struct {
	Configuration, Sessions, Credentials, CodexFile string
	Normal                                          bool
}

func absolutePath(path string) string {
	if path == "" {
		return ""
	}
	abs, _ := filepath.Abs(path)
	return abs
}

func checkLauncherHost(ctx context.Context, client *gateway.Client, o launchOptions) error {
	binding := launcherHostBinding{Configuration: o.config, Sessions: o.sessions, Credentials: o.credentials, CodexFile: o.codexFile, Normal: o.normal}
	request, err := json.Marshal(modelRequest{Action: "launcher.check", HostBinding: &binding})
	if err != nil {
		return err
	}
	raw, err := client.Extension(ctx, request)
	if err != nil {
		return errors.New("cannot verify Host configuration; stop the old Host and relaunch with the updated binary (Session history is preserved)")
	}
	var reply modelReply
	if json.Unmarshal(raw, &reply) != nil || reply.Error != "" {
		return errors.New("Host configuration does not match this launcher; use the matching config/store/socket or stop that Host before restarting (Session history is preserved)")
	}
	return nil
}
