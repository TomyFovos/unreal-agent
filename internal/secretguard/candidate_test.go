package secretguard

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestCredentialCandidatePreservesRegexpResults(t *testing.T) {
	cases := []string{"ordinary message", strings.Repeat("x", 4500), "token=abc", "access_token:abc", "REFRESH-TOKEN='abc'", "idtoken=abc", "oauthtoken=abc", "auth_token=abc", "api_key=abc", "AUTHORIZATION: Bearer abc", "client-secret=abc", "secret=abc", "password=abc", "private_key=abc", "ANTHROPIC_AUTH_TOKEN=abc", "CLAUDE_CODE_OAUTH_TOKEN=abc", "OPENAI_CODEX_ACCESS_TOKEN=abc", "Bearer abc", "sk-12345678", "eyJabc.def.ghi", "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----", "-----BEGIN EC PRIVATE KEY-----abc", "-----BEGIN OPENSSH PRIVATE KEY-----abc", "-----BEGIN PRIVATE KEY-----abc", "日本語 token=abc", "Xſk-12345678", "XſECRET=abc"}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		var b strings.Builder
		for range 100 {
			b.WriteByte(byte(32 + rng.IntN(95)))
		}
		cases = append(cases, b.String())
	}
	for _, text := range cases {
		if got, want := Credentials(text), pattern.MatchString(text); got != want {
			t.Fatalf("match changed for %q: %v vs %v", text, got, want)
		}
		if got, want := Redact(text), pattern.ReplaceAllString(text, "[REDACTED]"); got != want {
			t.Fatalf("redaction changed for %q", text)
		}
	}
}
