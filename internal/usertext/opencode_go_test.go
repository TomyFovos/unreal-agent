package usertext

import (
	"strings"
	"testing"
)

// OpenCode Go has no login command: its guidance points to /login, where the
// key-saving command is shown, and never to Codex or a CLI.
func TestOpenCodeGoFailuresPointToLogin(t *testing.T) {
	for _, tc := range []struct{ code, detail, want string }{
		{"unauthenticated", "status=401 stage=http_status reason=invalid_token", "/login の手順で保存し直して"},
		{"provider_unavailable", "", "/login の手順"},
		{"catalog_unavailable", "opencode_go_list_unreachable", "OpenCode Go のモデル一覧"},
		{"rate_limited", "status=429 reason=usage_limit_reached", "利用上限"},
	} {
		text := Failure(tc.code, tc.detail, "opencode-go")
		if !strings.Contains(text, tc.want) {
			t.Fatal(tc, text)
		}
		for _, gone := range []string{"codex", "CLI", "status=", "opencode_go_list"} {
			if strings.Contains(text, gone) {
				t.Fatal(gone, text)
			}
		}
	}
}
