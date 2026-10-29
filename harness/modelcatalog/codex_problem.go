package modelcatalog

// CodexProblemHint exposes only our closed categories, never subprocess text,
// paths, error payloads, account metadata or an untrusted Problem string.
func CodexProblemHint(code string) string {
	switch code {
	case "codex_cli_missing":
		return "Codex CLI をインストールしてから codex login でログインしてください"
	case "codex_cache_missing", "codex_cache_unreadable", "codex_cache_oversized", "codex_cache_invalid", "codex_cache_no_selectable_models":
		return "Codex の保存済みモデル一覧を読み取れません。Codex CLI の起動・ログイン状態を確認し、/model refresh を実行してください"
	case "codex_cli_start_failed", "codex_cli_timeout", "codex_cli_canceled", "codex_cli_rejected", "codex_cli_protocol_invalid", "codex_cli_metadata_invalid", "codex_cli_oversized":
		return "Codex のモデル一覧を取得できません。Codex CLI の起動・ログイン状態を確認し、/model refresh を実行してください"
	case "codex_oauth_source_invalid":
		return "Codex の接続を確認してから /model refresh を実行してください"
	default:
		return ""
	}
}
