package openaicodex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeCodexRouteDetection(t *testing.T) {
	for _, tc := range []struct {
		name, mode, proxy, auth, config string
		docker, refused                 bool
	}{
		{name: "external", auth: `{"tokens":{"access_token":"synthetic-private","account_id":"synthetic-private"}}`},
		{name: "proxy-string-alone", proxy: "http://host.docker.internal:3128", auth: `{"tokens":{}}`},
		{name: "hostname-label-irrelevant", auth: `{"tokens":{}}`, config: `model_provider="openai"`},
		{name: "full-docker", mode: "oauth", proxy: "http://host.docker.internal:3128", auth: `{"OPENAI_API_KEY":"proxy-managed"}`, config: sandboxTOML, docker: true},
		{name: "mode-alone-no-fallback", mode: "oauth", auth: `{"tokens":{"access_token":"synthetic-private"}}`, refused: true},
		{name: "kit-no-mode", proxy: "http://host.docker.internal:3128", auth: `{"OPENAI_API_KEY":"proxy-managed"}`, config: sandboxTOML, refused: true},
		{name: "missing-proxy", mode: "oauth", auth: `{"OPENAI_API_KEY":"proxy-managed"}`, config: sandboxTOML, refused: true},
		{name: "invalid-proxy", mode: "oauth", proxy: "http://PRIVATE_VALUE@evil.invalid:3128", auth: `{"OPENAI_API_KEY":"proxy-managed"}`, config: sandboxTOML, refused: true},
		{name: "api-kit", mode: "apikey", proxy: "http://host.docker.internal:3128", auth: `{"OPENAI_API_KEY":"proxy-managed"}`, config: sandboxTOML, refused: true},
		{name: "mixed-credentials", mode: "oauth", proxy: "http://host.docker.internal:3128", auth: `{"OPENAI_API_KEY":"proxy-managed","tokens":{}}`, config: sandboxTOML, refused: true},
		{name: "sandbox-routing-external-auth", auth: `{"tokens":{"access_token":"synthetic-private"}}`, config: sandboxTOML, refused: true},
		{name: "unknown-routing", auth: `{"tokens":{}}`, config: `model_provider="custom"`},
		{name: "malformed-routing", auth: `{"tokens":{}}`, config: `model_provider=[`, refused: true},
		{name: "malformed-auth", auth: `{"tokens":`, refused: true},
		{name: "no-files-external-validator-required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before := map[string][]byte{}
			for name, raw := range map[string]string{"auth.json": tc.auth, "config.toml": tc.config} {
				if raw == "" {
					continue
				}
				p := filepath.Join(dir, name)
				if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				before[p] = []byte(raw)
			}
			s := DockerSandboxSource{AuthFile: filepath.Join(dir, "auth.json"), Getenv: func(k string) string {
				switch k {
				case "SBX_CRED_OPENAI_MODE":
					return tc.mode
				case "HTTPS_PROXY":
					return tc.proxy
				case "HOSTNAME":
					return "asp"
				}
				return ""
			}}
			docker, err := s.DetectRoute()
			if docker != tc.docker || (err != nil) != tc.refused {
				t.Fatal("incorrect closed route decision", docker, err)
			}
			if err != nil {
				var safe *SandboxProxyError
				if !errors.As(err, &safe) || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatal("unsafe route error")
				}
			}
			for p, data := range before {
				got, _ := os.ReadFile(p)
				if string(got) != string(data) {
					t.Fatal("source modified")
				}
			}
		})
	}
}

func TestNativeCodexRouteRejectsUnsafeRoutingFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "writable", "oversized", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			switch kind {
			case "symlink":
				outside := filepath.Join(t.TempDir(), "target")
				os.WriteFile(outside, []byte(`model_provider="openai"`), 0600)
				os.Symlink(outside, path)
			case "writable":
				os.WriteFile(path, []byte(`model_provider="openai"`), 0666)
				os.Chmod(path, 0666)
			case "oversized":
				os.WriteFile(path, []byte(strings.Repeat("#x\n", 100000)), 0600)
			case "directory":
				os.Mkdir(path, 0700)
			}
			if _, err := (DockerSandboxSource{AuthFile: filepath.Join(dir, "auth.json"), Getenv: func(string) string { return "" }}).DetectRoute(); err == nil {
				t.Fatal("unsafe routing accepted")
			}
		})
	}
}
