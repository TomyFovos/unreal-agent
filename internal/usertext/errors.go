// Package usertext formats operational failures for ordinary product screens.
// Original errors remain in canonical history/diagnostics, never in this text.
package usertext

import "strings"

func Login(provider string) string {
	if strings.HasPrefix(provider, "claude") {
		return "claude を起動して /login"
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
	text := strings.ToLower(code + " " + detail)
	switch {
	case Authentication(code, detail):
		return "ログインが必要です。" + Login(provider) + " でログインし直すか、/model で切り替えて /resume。"
	case strings.Contains(text, "provider_unavailable"), strings.Contains(text, "cli_missing"), strings.Contains(text, "proxy_unavailable"):
		return "接続先を利用できません。" + Login(provider) + " を確認するか、/model で切り替えて /resume。"
	case strings.Contains(text, "catalog_unavailable"):
		return "モデル一覧を読み取れません。CLI の起動・ログイン状態を確認し、/model refresh を実行してください。"
	case strings.Contains(text, "disconnected"), strings.Contains(text, "transport_failed"):
		return "接続が切れました。自動で再接続します。"
	case strings.Contains(text, "writer_owned"):
		return "別の画面で使用中です。その画面を閉じてから /resume してください。"
	case strings.Contains(text, "rate_limit"), strings.Contains(text, "usage_limit"), strings.Contains(text, "status=429"), strings.Contains(text, "hit your limit"), strings.Contains(text, "usage limit"):
		return "利用上限に達しました。時間をおいて /resume するか、/model で切り替えてください。"
	case strings.Contains(text, "context_length"), strings.Contains(text, "context window"):
		return "会話がモデルの上限を超えました。/model で大きな文脈を扱えるモデルを選んで /resume してください。"
	case strings.Contains(text, "selection_changed"), strings.Contains(text, "selection_unavailable"), strings.Contains(text, "selection_failed"):
		return "モデルを変更できませんでした。/model を開き直して選んでください。"
	case strings.Contains(text, "tools unsupported"), strings.Contains(text, "text-only"):
		return "この接続先ではツールを使えません。/model で切り替えて /resume してください。"
	case strings.Contains(text, "forbidden"):
		return "このモデルを利用できません。/model で別のモデルを選んで /resume してください。"
	case strings.Contains(text, "permission"):
		return "操作が許可されませんでした。/permissions を確認するか、/model で切り替えて /resume してください。"
	case strings.Contains(text, "deadline"), strings.Contains(text, "timeout"):
		return "応答を待つ時間を超えました。/resume でもう一度試してください。"
	default:
		return "応答を取得できませんでした。/resume で再開するか、/model で切り替えてください。"
	}
}
