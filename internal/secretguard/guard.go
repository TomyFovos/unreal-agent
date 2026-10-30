// Package secretguard recognizes common credential-shaped text. It is a
// conservative exclusion guard, not a general-purpose secret detector.
package secretguard

import (
	"regexp"
	"strings"
)

var pattern = regexp.MustCompile(`(?i)(\b(?:access[_-]?token|refresh[_-]?token|id[_-]?token|oauth[_-]?token|auth[_-]?token|token|api[_-]?key|authorization|client[_-]?secret|secret|password|private[_-]?key|ANTHROPIC_AUTH_TOKEN|CLAUDE_CODE_OAUTH_TOKEN|OPENAI_CODEX_ACCESS_TOKEN)\b\s*["']?\s*[:=]\s*(?:bearer\s+)?[^\s,}\]]+|\bbearer\s+\S+|\bsk-[A-Za-z0-9_-]{8,}|\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?(?:-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|$))`)

// Presentation/export surfaces retain their existing identity exclusions.
// Model context uses Credentials so ordinary task metadata remains usable.
var identityPattern = regexp.MustCompile(`(?i)\b(?:account[_-]?id|org[_-]?(?:id|name)|email)\b\s*["']?\s*[:=]\s*[^\s,}\]]+`)

func Sensitive(text string) bool { return Credentials(text) || identityPattern.MatchString(text) }

func Credentials(text string) bool { return credentialCandidate(text) && pattern.MatchString(text) }

// CredentialPath names the provider-private files already excluded from model
// context. Read tools keep that exclusion when using ambient filesystem roots.
func CredentialPath(path string) bool {
	path = "/" + strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	return strings.Contains(path, "/.codex/auth.json") || strings.Contains(path, "/.claude/.credentials")
}

// Redact keeps ordinary instructions, including email addresses and file
// contents, while removing credential-shaped text before a model request.
func Redact(text string) string {
	if !credentialCandidate(text) {
		return text
	}
	return pattern.ReplaceAllString(text, "[REDACTED]")
}

// Every alternative in pattern contains at least one of these literal words.
// Searching them first avoids running the regexp machine over large ordinary
// messages. Non-ASCII text retains the original Unicode case-fold behavior.
func credentialCandidate(text string) bool {
	for i := range len(text) {
		if text[i] >= 0x80 {
			return true
		}
	}
	lower := strings.ToLower(text)
	for _, marker := range []string{"token", "key", "authorization", "secret", "password", "bearer", "sk-", "eyj"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
