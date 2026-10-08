package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
)

func codexReference() credential.Reference {
	return credential.Reference{Provider: "openai-codex", Method: credential.OAuth, ID: "external-codex"}
}

func writeCodexAuth(t *testing.T, path, token, account string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": map[string]string{"access_token": token, "account_id": account, "refresh_token": "unused-external-refresh-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestInteractiveCodexFileDiscovery(t *testing.T) {
	for _, test := range []struct {
		name, option, want string
		env                map[string]string
	}{
		{name: "default", env: map[string]string{"HOME": "/home/example"}, want: "/home/example/.codex/auth.json"},
		{name: "Codex home", env: map[string]string{"HOME": "/home/example", "CODEX_HOME": " /custom/codex "}, want: "/custom/codex/auth.json"},
		{name: "environment file", env: map[string]string{"CODEX_HOME": "/custom/codex", "OPENAI_CODEX_AUTH_FILE": " /custom/auth.json "}, want: "/custom/auth.json"},
		{name: "explicit option", option: "/option/auth.json", env: map[string]string{"OPENAI_CODEX_AUTH_FILE": "/other/auth.json"}, want: "/option/auth.json"},
		{name: "relative file", option: "auth.json", want: "auth.json"},
		{name: "inline credentials ignored", env: map[string]string{"HOME": "/home/example", "OPENAI_CODEX_ACCESS_TOKEN": "do-not-read-token", "OPENAI_CODEX_ACCOUNT_ID": "do-not-read-account", "OPENAI_API_KEY": "sk-do-not-read"}, want: "/home/example/.codex/auth.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := externalCodexAuthFile(test.option, func(key string) string {
				if key != "HOME" && key != "CODEX_HOME" && key != "OPENAI_CODEX_AUTH_FILE" {
					t.Error("discovery consulted credential material")
				}
				return test.env[key]
			})
			want, absErr := filepath.Abs(test.want)
			if err != nil || absErr != nil || path != want {
				t.Fatalf("unexpected auth source path: %v", err)
			}
		})
	}
}

func TestExternalCodexResolverRoutesOnlyExplicitReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeCodexAuth(t, path, "external-token", "external-account")
	managedCalls := 0
	managed := credential.ResolverFunc(func(_ context.Context, ref credential.Reference) (credential.Material, error) {
		managedCalls++
		if isExternalCodex(ref) {
			t.Fatal("external credential sent to managed store")
		}
		return credential.Material{Token: credential.NewSecret("managed-key"), Owner: credential.Managed}, nil
	})
	resolver := withExternalCodexCredentials(managed, path)
	material, err := resolver.Resolve(t.Context(), codexReference())
	if err != nil || material.Token.Reveal() != "external-token" || material.AccountID != "external-account" || material.Owner != credential.External || material.RefreshToken.Reveal() != "" {
		t.Fatalf("external resolution failed: %v", err)
	}
	for _, ref := range []credential.Reference{
		{Provider: "openai", Method: credential.APIKey, ID: "primary"},
		{Provider: "openai-codex", Method: credential.OAuth, ID: "other"},
		{Provider: "openai-codex", Method: credential.APIKey, ID: "external-codex"},
	} {
		material, err = resolver.Resolve(t.Context(), ref)
		if err != nil || material.Token.Reveal() != "managed-key" || material.Owner != credential.Managed {
			t.Fatalf("managed resolution changed: %v", err)
		}
	}
	if managedCalls != 3 || withExternalCodexCredentials(nil, "") != nil {
		t.Fatal("unexpected resolver routing")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = resolver.Resolve(ctx, codexReference()); !errors.Is(err, context.Canceled) {
		t.Fatal("external resolver lost cancellation")
	}
}

func TestChildCodexPathMustBeAbsolute(t *testing.T) {
	if err := runChild(t.Context(), []string{"--stdio", "--codex-auth-file", "relative/auth.json"}, os.Stderr); err == nil || err.Error() != "child Codex auth file path must be absolute" {
		t.Fatalf("relative child auth path accepted: %v", err)
	}
}
