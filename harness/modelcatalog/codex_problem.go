package modelcatalog

// CodexProblemHint exposes only our closed categories, never subprocess text,
// paths, error payloads, account metadata or an untrusted Problem string.
func CodexProblemHint(code string) string {
	switch code {
	case "codex_cache_missing":
		return "Codex CLI model cache missing"
	case "codex_cache_unreadable":
		return "Codex CLI model cache unreadable"
	case "codex_cache_oversized":
		return "Codex CLI model cache exceeds size limit"
	case "codex_cache_invalid":
		return "Codex CLI model cache format invalid"
	case "codex_cache_no_selectable_models":
		return "Codex CLI cache has no supported selectable models"
	case "codex_cli_missing":
		return "Codex CLI not found for model/list"
	case "codex_cli_start_failed":
		return "Codex model/list process unavailable"
	case "codex_cli_timeout":
		return "Codex model/list timed out"
	case "codex_cli_canceled":
		return "Codex model/list canceled"
	case "codex_cli_rejected":
		return "Codex model/list request rejected"
	case "codex_cli_protocol_invalid":
		return "Codex model/list protocol incompatible"
	case "codex_cli_metadata_invalid":
		return "Codex model/list metadata invalid"
	case "codex_cli_oversized":
		return "Codex model/list exceeds size/count limit"
	case "codex_oauth_source_invalid":
		return "Docker OAuth source validation failed"
	default:
		return ""
	}
}
