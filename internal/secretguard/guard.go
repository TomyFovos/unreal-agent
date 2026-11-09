// Package secretguard recognizes common credential-shaped text. It is a
// conservative exclusion guard, not a general-purpose secret detector.
package secretguard

import (
	"regexp"
	"slices"
	"strings"
)

const privateKeyExpression = `-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?(?:-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|$)`

var pattern = regexp.MustCompile(`(?i)(\b(?:access[_-]?token|refresh[_-]?token|id[_-]?token|oauth[_-]?token|auth[_-]?token|token|api[_-]?key|authorization|client[_-]?secret|secret|password|private[_-]?key|ANTHROPIC_AUTH_TOKEN|CLAUDE_CODE_OAUTH_TOKEN|OPENAI_CODEX_ACCESS_TOKEN)\b\s*["']?\s*[:=]\s*(?:bearer\s+)?[^\s,}\]]+|\bbearer\s+\S+|\bsk-[A-Za-z0-9_-]{8,}|\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|` + privateKeyExpression + `)`)

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
	return redact(text, pattern)
}

// RedactSensitive also masks identity fields on presentation/export surfaces.
// Model context uses Redact so ordinary task metadata remains available.
func RedactSensitive(text string) string {
	return redact(text, pattern, identityPattern)
}

func redact(text string, patterns ...*regexp.Regexp) string {
	var matches [][]int
	for _, p := range patterns {
		for offset := 0; offset < len(text); {
			match := p.FindStringIndex(text[offset:])
			if match == nil {
				break
			}
			match[0] += offset
			match[1] += offset
			matches = append(matches, match)
			// Assignment/Bearer matches can consume the start of another
			// credential, including a PEM header. Cover overlapping matches.
			offset = match[0] + 1
		}
	}
	if len(matches) == 0 {
		return text
	}
	// Merge overlapping ranges before replacing, retaining all other text.
	slices.SortFunc(matches, func(a, b []int) int { return a[0] - b[0] })
	var out strings.Builder
	end := 0
	for _, match := range matches {
		if match[0] >= end {
			out.WriteString(text[end:match[0]])
			out.WriteString("[REDACTED]")
		}
		end = max(end, match[1])
	}
	out.WriteString(text[end:])
	return out.String()
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
