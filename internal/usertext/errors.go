// Package usertext formats operational failures for ordinary product screens.
// Original errors remain in canonical history/diagnostics, never in this text.
package usertext

import (
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
	"strings"
)

func Login(provider string) string {
	if strings.HasPrefix(provider, "claude") {
		return "claude を起動して /login"
	}
	if provider == "opencode-go" {
		return "/login の手順"
	}
	return "codex login"
}
func Authentication(code, detail string) bool {
	text := strings.ToLower(code + " " + detail)
	for _, key := range []string{"external_reauth_required", "auth_required", "unauthenticated", "authentication", "invalid_token", "status=401"} {
		if strings.Contains(text, key) {
			return true
		}
	}
	return false
}
func Failure(code, detail, provider string) string {
	return failure(code, detail, provider, false)
}

func TurnFailure(detail, provider string) string {
	return failure("", detail, provider, true)
}

func failure(code, detail, provider string, live bool) string {
	text := strings.ToLower(code + " " + detail)
	withAction := func(reason, whenStopped, whileRunning string) string {
		if live {
			return reason + whileRunning
		}
		return reason + whenStopped
	}
	switch {
	case strings.HasPrefix(detail, "fast_unsupported: fast unsupported for "):
		return strings.TrimPrefix(detail, "fast_unsupported: ")
	case strings.Contains(text, buildfeatures.OpenCodeGoDisabledCode):
		return buildfeatures.OpenCodeGoDisabledMessage
	case Authentication(code, detail) && provider == "opencode-go":
		// The key is read for each request, so saving it again needs no restart.
		return withAction("API キーが使えません。/login の手順で保存し直してから ", "/resume するか、/model で切り替えてください。", "入力を送り直すか、/model で切り替えてください。")
	case Authentication(code, detail):
		return withAction("ログインが必要です。"+Login(provider)+" でログインし直すか、/model で切り替えて ", "/resume。", "入力を送り直してください。")
	case strings.Contains(text, "provider_unavailable"), strings.Contains(text, "cli_missing"), strings.Contains(text, "proxy_unavailable"):
		return withAction("接続先を利用できません。"+Login(provider)+" を確認するか、/model で切り替えて ", "/resume。", "入力を送り直してください。")
	case strings.Contains(text, "catalog_unavailable") && provider == "opencode-go":
		return "OpenCode Go のモデル一覧を読み取れません。通信を確認し、/model refresh を実行してください。"
	case strings.Contains(text, "catalog_unavailable"):
		return "モデル一覧を読み取れません。CLI の起動・ログイン状態を確認し、/model refresh を実行してください。"
	case strings.Contains(text, "disconnected"), strings.Contains(text, "transport_failed"):
		return "接続が切れました。自動で再接続します。"
	case strings.Contains(text, "writer_owned"):
		return "別の画面で使用中です。その画面を閉じてから /resume してください。"
	case strings.Contains(text, "rate_limit"), strings.Contains(text, "usage_limit"), strings.Contains(text, "status=429"), strings.Contains(text, "hit your limit"), strings.Contains(text, "usage limit"):
		return withAction("利用上限に達しました。時間をおいて ", "/resume するか、/model で切り替えてください。", "入力を送り直すか、/model で切り替えてください。")
	case strings.Contains(text, "context_length"), strings.Contains(text, "context window"), strings.Contains(text, "required_context_exceeds_budget"):
		return withAction("会話がモデルの上限を超えました。/model で大きな文脈を扱えるモデルを選んで ", "/resume してください。", "入力を送り直してください。")
	case strings.Contains(text, "selection_changed"), strings.Contains(text, "selection_unavailable"), strings.Contains(text, "selection_failed"):
		return "モデルを変更できませんでした。/model を開き直して選んでください。"
	case strings.Contains(text, "tools unsupported"), strings.Contains(text, "text-only"):
		return withAction("この接続先ではツールを使えません。/model で切り替えて ", "/resume してください。", "入力を送り直してください。")
	case strings.Contains(text, "forbidden"):
		return withAction("このモデルを利用できません。/model で別のモデルを選んで ", "/resume してください。", "入力を送り直してください。")
	case strings.Contains(text, "permission"):
		return withAction("操作が許可されませんでした。/permissions を確認するか、/model で切り替えて ", "/resume してください。", "入力を送り直してください。")
	case strings.Contains(text, "deadline"), strings.Contains(text, "timeout"):
		return withAction("応答を待つ時間を超えました。", "/resume でもう一度試してください。", "入力を送り直してください。")
	default:
		return withAction("応答を取得できませんでした。", "/resume で再開するか、/model で切り替えてください。", "入力を送り直すか、/model で切り替えてください。")
	}
}
