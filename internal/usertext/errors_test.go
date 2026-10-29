package usertext

import (
	"strings"
	"testing"
)

func TestFailuresHaveActionAndHideInternalDetails(t *testing.T) {
	cases := []struct{ code, detail, want string }{
		{"unauthenticated", "status=401 stage=http_status reason=invalid_token", "codex login"},
		{"catalog_unavailable", "source=/private/models_cache.json", "CLI の起動・ログイン状態"},
		{"transport_failed", "socket=/private/host.sock", "自動で再接続"},
		{"rate_limited", "status=429", "利用上限"},
		{"turn_failed", "You've hit your limit", "利用上限"},
		{"invalid_request", "stage=request_build", "/model"},
	}
	for _, tc := range cases {
		text := Failure(tc.code, tc.detail, "openai-codex")
		if !strings.Contains(text, tc.want) || strings.ContainsAny(text, "\r\n") {
			t.Fatal(tc, text)
		}
		for _, hidden := range []string{"status=", "stage=", "/private/", tc.code} {
			if strings.Contains(text, hidden) {
				t.Fatal("internal detail", text)
			}
		}
	}
	if text := Failure("external_reauth_required", "", "claude"); !strings.Contains(text, "claude を起動して /login") || !strings.Contains(text, "/model") {
		t.Fatal(text)
	}
}
