package secretguard

import (
	"strings"
	"testing"
)

func TestRedactPreservesSurroundingTextAndCoversCredentials(t *testing.T) {
	cases := []string{
		"token: str", "secret = load()", "access_token=synthetic-value", "refresh-token='synthetic-value'",
		"idtoken=synthetic-value", "oauth_token=synthetic-value", "auth-token=synthetic-value",
		`"api_key": "synthetic-value"`, "Authorization: Bearer synthetic-value", "client_secret=synthetic-value",
		"password=synthetic-value", "private_key=synthetic-value", "Bearer synthetic-value",
		"ANTHROPIC_AUTH_TOKEN=synthetic-value", "CLAUDE_CODE_OAUTH_TOKEN=synthetic-value", "OPENAI_CODEX_ACCESS_TOKEN=synthetic-value",
		"sk-synthetic-value", "eyJsynthetic.value.signature", "XſECRET=synthetic-value",
		"Bearer Bearer synthetic-value", "token=secret= synthetic-value", "Bearer secret= synthetic-value",
	}
	for _, kind := range []string{"", "RSA ", "EC ", "OPENSSH "} {
		for _, prefix := range []string{"", "private_key=", `"private_key": "`, "Bearer ", "Authorization: Bearer "} {
			key := "-----BEGIN " + kind + "PRIVATE KEY-----\nsynthetic-value\n-----END " + kind + "PRIVATE KEY-----"
			cases = append(cases, prefix+key, prefix+strings.Split(key, "\n-----END")[0])
		}
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			if !Credentials(text) {
				t.Fatal("fixture must match the credential guard")
			}
			got := Redact("keep before\n" + text + "\nkeep after")
			if !strings.HasPrefix(got, "keep before\n") || !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "synthetic-value") || Credentials(got) || Redact(got) != got {
				t.Fatalf("incomplete or unstable redaction: %q", got)
			}
			// An unterminated PEM deliberately consumes the remaining text.
			if !strings.Contains(text, "-----BEGIN ") || strings.Contains(text, "-----END ") {
				if !strings.HasSuffix(got, "\nkeep after") {
					t.Fatalf("lost surrounding text: %q", got)
				}
			}
		})
	}
	const ordinary = "Keep code.py and email=developer@example.com account_id=developer."
	if Redact(ordinary) != ordinary {
		t.Fatal("model context lost ordinary metadata")
	}
	if got := RedactSensitive(ordinary); got != "Keep code.py and [REDACTED] [REDACTED]" || Sensitive(got) {
		t.Fatalf("presentation identity redaction: %q", got)
	}
}

func FuzzRedactLeavesNoDetectedCredentials(f *testing.F) {
	for _, text := range []string{"ordinary source", "token: str", "secret = load()", "token=Bearer synthetic-value", "Bearer Bearer synthetic-value", "token=secret= synthetic-value", "private_key=-----BEGIN PRIVATE KEY-----\nsynthetic-value\n-----END PRIVATE KEY-----", "Bearer -----BEGIN RSA PRIVATE KEY-----\nsynthetic-value"} {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		got := Redact(text)
		if Credentials(got) || Redact(got) != got || Credentials(text) && got == text {
			t.Fatalf("redaction left detected credentials or was unstable: %q -> %q", text, got)
		}
	})
}
