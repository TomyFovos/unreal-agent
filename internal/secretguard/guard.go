// Package secretguard recognizes common credential-shaped text. It is a
// conservative exclusion guard, not a general-purpose secret detector.
package secretguard

import "regexp"

var pattern = regexp.MustCompile(`(?i)(\b(?:access[_-]?token|refresh[_-]?token|id[_-]?token|oauth[_-]?token|auth[_-]?token|token|api[_-]?key|authorization|client[_-]?secret|secret|password|private[_-]?key|ANTHROPIC_AUTH_TOKEN|CLAUDE_CODE_OAUTH_TOKEN|OPENAI_CODEX_ACCESS_TOKEN)\b\s*["']?\s*[:=]\s*(?:bearer\s+)?[^\s,}\]]+|\bbearer\s+\S+|\bsk-[A-Za-z0-9_-]{8,}|\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?(?:-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|$))`)

// Presentation/export surfaces retain their existing identity exclusions.
// Model context uses Credentials so ordinary task metadata remains usable.
var identityPattern = regexp.MustCompile(`(?i)\b(?:account[_-]?id|org[_-]?(?:id|name)|email)\b\s*["']?\s*[:=]\s*[^\s,}\]]+`)

func Sensitive(text string) bool { return Credentials(text) || identityPattern.MatchString(text) }

func Credentials(text string) bool { return pattern.MatchString(text) }

// Redact keeps ordinary instructions, including email addresses and file
// contents, while removing credential-shaped text before a model request.
func Redact(text string) string { return pattern.ReplaceAllString(text, "[REDACTED]") }
