// Package testclaude builds a native fake Claude executable for offline tests.
// The production adapter still launches the configured binary directly.
package testclaude

import (
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type Config struct {
	Version, MissingFlag, AuthMethod, APIProvider, Subscription, Stream string
	SignedOut                                                           bool
	Exit                                                                int
	Wait, Child, IgnoreTermination                                      bool
	Gate                                                                string
	Doctor                                                              string
	ManagedTelemetry                                                    bool
	AuthStatus                                                          string
	Catalog, CatalogStream                                              string
	CatalogWait, CatalogChild, InitializeModelsOnly                     bool
	BridgeSteps                                                         []BridgeStep
	BridgeChildSteps                                                    []BridgeStep
	BridgeSource, BridgeEvent                                           string
	BridgeExtraServer                                                   bool
	BridgeOnce                                                          bool
	BridgeManagedPermissionsOnly                                        bool
	BridgePermissionRules                                               string
	StructuredResponses                                                 []string
	StructuredHelperInputs                                              []string
	StructuredAssistantFrames                                           []string
	StructuredInitializeError                                           bool
	StructuredSerializerFailures                                        int
	StructuredEnforcementReminder                                       bool
}
type BridgeStep struct {
	Name, Arguments, Contains           string
	Error, Duplicate, DuplicateEnvelope bool
}
type Call struct {
	Arguments, Environment   []string
	Input, System, Directory string
	PID, ChildPID            int
	Schema                   string
	// Synthetic fixture traffic only; never used by the real provider probe.
	Initialize, UserFrame string `json:",omitempty"`
}
type Fixture struct{ Binary, Home, Directory string }

var buildOnce sync.Once
var executable []byte
var buildErr error

func New(t testing.TB) *Fixture {
	t.Helper()
	buildOnce.Do(func() {
		_, source, _, _ := runtime.Caller(0)
		dir, err := os.MkdirTemp("", "unreal-fake-claude-build-")
		if err != nil {
			buildErr = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "claude")
		cmd := exec.Command("go", "build", "-o", path, ".")
		cmd.Dir = filepath.Join(filepath.Dir(source), "cmd")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Log(string(out))
			return
		}
		executable, buildErr = os.ReadFile(path)
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	dir := t.TempDir()
	f := &Fixture{Directory: dir, Binary: filepath.Join(dir, "claude"), Home: filepath.Join(dir, "home")}
	if err := os.Mkdir(f.Home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Binary, executable, 0700); err != nil {
		t.Fatal(err)
	}
	f.Set(t, Config{})
	return f
}
func (f *Fixture) Set(t testing.TB, c Config) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.Directory, "fake-next.json")
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(p, filepath.Join(f.Directory, "fake.json")); err != nil {
		t.Fatal(err)
	}
}
func (f *Fixture) Getenv(key string) string {
	if key == "HOME" {
		return f.Home
	}
	return ""
}
func (f *Fixture) Calls(t testing.TB) []Call {
	t.Helper()
	return f.calls(t, "calls.jsonl")
}

// AuthCalls records invocation metadata, never auth-status output or credentials.
func (f *Fixture) AuthCalls(t testing.TB) []Call {
	t.Helper()
	return f.calls(t, "auth-calls.jsonl")
}

func (f *Fixture) CatalogCalls(t testing.TB) []Call {
	t.Helper()
	return f.calls(t, "catalog-calls.jsonl")
}

func (f *Fixture) StructuredProbes(t testing.TB) []Call {
	t.Helper()
	return f.calls(t, "structured-probes.jsonl")
}

func (f *Fixture) calls(t testing.TB, name string) []Call {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.Directory, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []Call
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var c Call
		if err = json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}
