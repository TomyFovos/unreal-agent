package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/host/gateway"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudecode"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

type launchOptions struct {
	config, state, sessions, socket, credentials, codexFile string
	id                                                      session.ID
	timeout                                                 time.Duration
	help                                                    bool
	normal                                                  bool
}

// Dependency hooks are local composition, not Host or gateway state.
type launcher struct {
	start  func(launchOptions) (*backgroundHost, error)
	attach func(context.Context, *gateway.Client, session.ID) error
	choose initialProviderChooser
}

func runLauncher(ctx context.Context, args []string, output io.Writer) error {
	return (launcher{start: startBackgroundHost, attach: attachViewer}).run(ctx, args, output)
}

func (l launcher) run(ctx context.Context, args []string, output io.Writer) error {
	o, err := parseLaunchOptions(args, output, os.Getenv)
	if err != nil || o.help {
		return err
	}
	client := gateway.NewClient(o.socket)
	defer client.Close()
	if l.choose == nil {
		l.choose = chooseInitialProvider(output)
	}
	// Human first-run selection is not gateway readiness work. Finish it under
	// the same startup lock before starting the bounded readiness deadline.
	if err = l.prepareNormalConfig(ctx, client, o); err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, o.timeout)
	err = l.ensureHost(startup, client, o)
	cancel()
	if err != nil {
		return err
	}
	if err = checkLauncherHost(ctx, client, o); err != nil {
		return err
	}
	view, err := openLauncherSession(ctx, client, o.id)
	if err != nil {
		return err
	}
	return l.attach(ctx, client, view.Session.Session.ID)
}

func (l launcher) prepareNormalConfig(ctx context.Context, client *gateway.Client, o launchOptions) error {
	if !o.normal {
		return nil
	}
	if _, err := os.Lstat(o.config); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if probeHost(ctx, client) == nil {
		return nil // an existing owner keeps its original startup configuration
	}
	if err := makePrivateDirectory(filepath.Dir(o.socket)); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		lock, err := tryStartupLock(o.socket + ".start.lock")
		if err != nil {
			return err
		}
		if lock != nil {
			defer lock.Close()
			if probeHost(ctx, client) == nil {
				return nil // an owner became ready while this launcher waited
			}
			_, err = resolveLauncherConfig(ctx, o, l.choose)
			return err
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-tick.C:
		}
	}
}

func parseLaunchOptions(args []string, output io.Writer, getenv func(string) string) (launchOptions, error) {
	o := launchOptions{timeout: 20 * time.Second, id: "default"}
	flags := flag.NewFlagSet("unreal", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&o.config, "config", "", "operator runtime JSON (default ~/.config/unreal-agent/runtime.json)")
	flags.StringVar(&o.state, "state-directory", "", "private state (default ~/.local/state/unreal-agent)")
	flags.StringVar(&o.sessions, "session-directory", "", "canonical sessions (default STATE/sessions)")
	flags.StringVar(&o.socket, "socket", "", "private socket (default XDG_RUNTIME_DIR/unreal-agent/host.sock or STATE/run/host.sock)")
	flags.StringVar(&o.credentials, "credential-directory", "", "optional managed API-key store")
	flags.StringVar(&o.codexFile, "codex-auth-file", "", "external Codex auth file; uses existing Codex discovery by default")
	flags.DurationVar(&o.timeout, "startup-timeout", o.timeout, "maximum wait for gateway readiness")
	flags.BoolVar(&o.help, "help", false, "show usage")
	flags.BoolVar(&o.help, "h", false, "show usage")
	flags.Usage = func() {
		fmt.Fprintln(output, "usage: unreal [options] [SESSION_NAME]\n\nNo name uses session default. Existing sessions attach or resume automatically.")
		flags.PrintDefaults()
	}
	// Keep SESSION_NAME positional while permitting known options on either side.
	var options, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		options = append(options, a)
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := flags.Lookup(name)
		if f != nil && name != "help" && name != "h" && !hasValue && i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	if err := flags.Parse(options); err != nil {
		return o, err
	}
	if o.help {
		flags.Usage()
		return o, nil
	}
	if len(positional) > 1 {
		return o, errors.New("usage: unreal [options] [SESSION_NAME]; provide at most one session name")
	}
	if len(positional) == 1 {
		o.id = session.ID(positional[0])
	}
	if err := validateLaunchName(string(o.id)); err != nil {
		return o, err
	}
	if o.timeout <= 0 {
		return o, errors.New("startup-timeout must be positive")
	}
	home := getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return o, err
		}
	}
	base := func(key, fallback string) (string, error) {
		value := getenv(key)
		if value == "" {
			value = fallback
		}
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("%s must be an absolute directory", key)
		}
		return value, nil
	}
	if o.config == "" {
		o.normal = true
		root, err := base("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		if err != nil {
			return o, err
		}
		o.config = filepath.Join(root, "unreal-agent", "runtime.json")
	}
	if o.state == "" {
		root, err := base("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
		if err != nil {
			return o, err
		}
		o.state = filepath.Join(root, "unreal-agent")
	}
	if o.sessions == "" {
		o.sessions = filepath.Join(o.state, "sessions")
	}
	if o.socket == "" {
		runtime := getenv("XDG_RUNTIME_DIR")
		if runtime != "" {
			if !filepath.IsAbs(runtime) {
				return o, errors.New("XDG_RUNTIME_DIR must be absolute")
			}
			if _, err := os.Lstat(runtime); err == nil {
				if err = checkPrivateDirectory(runtime); err != nil {
					return o, err
				}
				o.socket = filepath.Join(runtime, "unreal-agent", "host.sock")
			} else if !errors.Is(err, fs.ErrNotExist) {
				return o, err
			}
		}
		if o.socket == "" {
			o.socket = filepath.Join(o.state, "run", "host.sock")
		}
	}
	for _, path := range []*string{&o.config, &o.state, &o.sessions, &o.socket, &o.credentials, &o.codexFile} {
		if *path == "" {
			continue
		}
		absolute, err := filepath.Abs(*path)
		if err != nil {
			return o, err
		}
		*path = absolute
	}
	if len(o.socket) > 100 {
		return o, errors.New("socket path exceeds 100 bytes; choose a shorter private directory with --socket")
	}
	return o, nil
}

func validateLaunchName(name string) error {
	if name == "" || len(name) > 128 || strings.HasPrefix(name, "-") {
		return errors.New("session name must be 1–128 ASCII letters, digits or dashes, and must not start with a dash")
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			continue
		}
		return errors.New("session name must contain only ASCII letters, digits and dashes")
	}
	return nil
}

func probeHost(ctx context.Context, client *gateway.Client) error {
	probe, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	methods, err := client.Methods(probe) // Existing bounded, read-only gateway method.
	if err == nil && len(methods) == 0 {
		return errors.New("Host did not return a valid gateway readiness response")
	}
	return err
}

func (l launcher) ensureHost(ctx context.Context, client *gateway.Client, o launchOptions) error {
	if err := makePrivateDirectory(filepath.Dir(o.socket)); err != nil {
		return err
	}
	if info, err := os.Lstat(o.socket); err == nil && info.Mode()&os.ModeSocket == 0 {
		return errors.New("refusing to replace a non-socket at the Host socket path")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var lock *os.File
	defer func() {
		if lock != nil {
			lock.Close()
		}
	}()
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	var child *backgroundHost
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("Host did not become ready; check %s or the existing Host at %s: %w", filepath.Join(o.state, "host.log"), o.socket, err)
		}
		if probeHost(ctx, client) == nil {
			return checkPrivateSocket(o.socket)
		}
		if lock == nil {
			var err error
			lock, err = tryStartupLock(o.socket + ".start.lock")
			if err != nil {
				return err
			}
		}
		if lock != nil && child == nil {
			owned, err := gatewaySocketOwned(o.socket + ".lock")
			if err != nil {
				return err
			}
			if !owned {
				config, err := resolveLauncherConfig(ctx, o, l.choose)
				if errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("runtime configuration is missing at %s; place an explicit runtime and permission policy there, or use --config (see docs/interactive.md): %w", o.config, err)
				}
				if err != nil {
					return fmt.Errorf("cannot load runtime configuration: %w", err)
				}
				if needsExternalCodex(config) && !o.normal && config.Launcher == nil && len(config.Providers) == 0 {
					codexPath, e := externalCodexAuthFile(o.codexFile, os.Getenv)
					err = e
					if err != nil {
						return err
					}
					// Inspect metadata only. Credential parsing/refresh ownership stays
					// with ExternalCodex, which rereads material on each request.
					info, e := os.Stat(codexPath)
					if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
						return fmt.Errorf("Codex credentials at %s must be an existing private regular file; use codex login and private permissions: %w", codexPath, &credential.Error{Code: "external_reauth_required"})
					}
				}
				if config.Runtime.Provider.Provider == "claude-code" && !o.normal && config.Launcher == nil && len(config.Providers) == 0 {
					if err = preflightClaudeLauncher(ctx, config); err != nil {
						return err
					}
				}
				if err = makePrivateDirectory(o.state); err != nil {
					return err
				}
				if err = makePrivateDirectory(o.sessions); err != nil {
					return err
				}
				child, err = l.start(o)
				if err != nil {
					return fmt.Errorf("could not start background Host: %w", err)
				}
			}
		}
		if child != nil {
			select {
			case <-child.done:
				err := child.waitErr
				// A manual serve may have won the authoritative gateway lock.
				if probeHost(ctx, client) == nil {
					return checkPrivateSocket(o.socket)
				}
				owned, lockErr := gatewaySocketOwned(o.socket + ".lock")
				if lockErr != nil {
					return lockErr
				}
				if !owned {
					if err == nil {
						err = errors.New("serve exited before readiness")
					}
					return fmt.Errorf("background Host startup failed; see %s: %w", filepath.Join(o.state, "host.log"), err)
				}
				child = &backgroundHost{} // Existing owner is starting; just wait.
			default:
			}
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

func openLauncherSession(ctx context.Context, client *gateway.Client, id session.ID) (host.View, error) {
	for attempt := 0; attempt < 4; attempt++ {
		view, err := client.Inspect(ctx, id, 0, 128)
		if err == nil && view.Running {
			return view, nil
		}
		mode := host.Resume
		if errors.Is(err, fs.ErrNotExist) {
			// Inspect covers sessions loaded by this Host. Existing atomic open
			// distinguishes a new session from saved state after a Host restart.
			mode = host.CreateOrResume
		} else if err != nil {
			return host.View{}, launcherSessionError(id, err)
		}
		view, err = client.Open(ctx, mode, id)
		if err == nil {
			return view, nil
		}
		if errors.Is(err, localfile.ErrWriterOwned) {
			current, e := client.Inspect(ctx, id, 0, 128)
			if e == nil && current.Running {
				return current, nil
			}
			if e == nil {
				continue
			}
			return host.View{}, launcherSessionError(id, err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return host.View{}, launcherSessionError(id, err)
		}
	}
	return host.View{}, launcherSessionError(id, localfile.ErrWriterOwned)
}

func launcherSessionError(id session.ID, cause error) error {
	message := fmt.Sprintf("cannot open session %q; check the Host runtime configuration and permissions", id)
	if errors.Is(cause, localfile.ErrWriterOwned) {
		message = fmt.Sprintf("session %q is currently owned by another writer", id)
	} else if errors.Is(cause, host.ErrStopped) {
		message = fmt.Sprintf("session %q stopped while connecting; run unreal %s again", id, id)
	} else {
		var e *gateway.Error
		if errors.As(cause, &e) && (e.Code == "disconnected" || e.Code == "transport_failed") {
			message = fmt.Sprintf("Host disconnected while opening session %q; run unreal %s again", id, id)
		}
	}
	return &launchError{message: message, cause: cause}
}

type launchError struct {
	message string
	cause   error
}

func preflightClaudeLauncher(ctx context.Context, config serveConfiguration) error {
	if config.Runtime.ClaudeCode == nil {
		return errors.New("claude-code requires Runtime.ClaudeCode executable/catalog configuration")
	}
	policy, err := permission.New(config.Permissions)
	if err != nil {
		return err
	}
	defer policy.Close()
	cli, err := claudecode.NewClient(*config.Runtime.ClaudeCode)
	if err != nil {
		return err
	}
	return cli.Probe(permission.WithPolicy(ctx, policy))
}

func (e *launchError) Error() string { return e.message }
func (e *launchError) Unwrap() error { return e.cause }
