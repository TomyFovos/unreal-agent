package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
)

// Compared through the existing private extension API. No credential values,
// daemon metadata file, or replacement gateway protocol is introduced.
type launcherHostBinding struct {
	Configuration, Sessions, Credentials, CodexFile string
	Normal                                          bool
	Identity                                        launcherIdentity
}

// Executable digest binds this exact build, including dirty-source changes.
// It is not credential material. Linux /proc/self/exe keeps the running inode
// even if its original pathname has been replaced by a newer installation.
type launcherIdentity struct {
	Protocol uint32
	Build    string
}

var ownLauncherIdentity = sync.OnceValue(func() launcherIdentity {
	path, err := os.Executable()
	if err != nil {
		return launcherIdentity{}
	}
	if runtime.GOOS == "linux" {
		path = "/proc/self/exe"
	}
	f, err := os.Open(path)
	if err != nil {
		return launcherIdentity{}
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return launcherIdentity{}
	}
	return launcherIdentity{Protocol: 1, Build: hex.EncodeToString(h.Sum(nil))}
})

func absolutePath(path string) string {
	if path == "" {
		return ""
	}
	abs, _ := filepath.Abs(path)
	return abs
}

func checkLauncherHost(ctx context.Context, client *gateway.Client, o launchOptions) error {
	binding := launcherHostBinding{Configuration: o.config, Sessions: o.sessions, Credentials: o.credentials, CodexFile: o.codexFile, Normal: o.normal, Identity: ownLauncherIdentity()}
	if binding.Identity.Build == "" {
		return errors.New("cannot verify launcher build identity; no Host started or stopped")
	}
	request, err := json.Marshal(modelRequest{Action: "launcher.check", HostBinding: &binding})
	if err != nil {
		return err
	}
	raw, err := client.Extension(ctx, request)
	if err != nil {
		return errors.New("cannot verify Host build/protocol/configuration; keep the old Host/history and use a separately configured state/socket, or explicitly stop that Host before updating")
	}
	var reply modelReply
	if json.Unmarshal(raw, &reply) != nil || reply.Error != "" {
		return errors.New("Host configuration does not match this launcher (including build/protocol); keep its history and use a separate state/socket or explicitly stop that Host before updating")
	}
	return nil
}
