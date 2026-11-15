package credential

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitAPIKeySource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	write := func(value string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	source := Source{File: path}
	ref := Reference{Provider: "openai", ID: ExplicitAPIKeyID, Method: APIKey}
	reads := 0
	sources := map[string]Source{"openai": source}
	resolve := WithSources(nil, sources, func(name string) string {
		reads++
		if name != "CHOSEN_KEY" {
			t.Error("read unselected environment variable")
		}
		return "ambient-fake-key"
	})
	identity, _ := json.Marshal(ref)
	if strings.Contains(string(identity), path) {
		t.Fatal("mutable source entered Session identity")
	}
	for _, value := range []string{"first-fake-key", "rotated-fake-key"} {
		write(value+"\n", 0o600)
		m, err := resolve.Resolve(t.Context(), ref)
		if err != nil || m.Token.Reveal() != value || m.Owner != External {
			t.Fatal("explicit file resolution failed")
		}
		encoded, _ := json.Marshal(m)
		if strings.Contains(string(encoded)+fmt.Sprintf("%v %#v", m.Token, m.Token), value) {
			t.Fatal("secret exposed")
		}
	}
	if reads != 0 {
		t.Fatal("file source read environment")
	}
	write("private-fake-key", 0o644)
	if _, err := resolve.Resolve(t.Context(), ref); !IsCode(err, "api_key_file_requires_0600") {
		t.Fatal("public file accepted")
	}
	write("", 0o600)
	if _, err := resolve.Resolve(t.Context(), ref); !IsCode(err, "api_key_missing_or_invalid") {
		t.Fatal("empty file accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve.Resolve(t.Context(), ref); !IsCode(err, "api_key_file_unreadable") {
		t.Fatal("missing file accepted")
	}
	delete(sources, "openai")
	if _, err := resolve.Resolve(t.Context(), ref); !IsCode(err, "api_key_source_unconfigured") || reads != 0 {
		t.Fatal("ambient API key used")
	}
	sources["openai"] = Source{Env: "CHOSEN_KEY"}
	if _, err := resolve.Resolve(t.Context(), ref); err != nil || reads != 1 {
		t.Fatal("explicit environment not read")
	}
	ref.Method = OAuth
	if _, err := resolve.Resolve(t.Context(), ref); !IsCode(err, "store_required") || reads != 1 {
		t.Fatal("API source read on subscription")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := source.Read(ctx, nil); err != context.Canceled {
		t.Fatal("cancellation ignored")
	}
}

func TestAPIKeySourceValidation(t *testing.T) {
	for _, source := range []Source{{}, {File: "relative"}, {File: "/key", Env: "KEY"}, {Env: "BAD=NAME"}} {
		if source.Validate() == nil {
			t.Fatal("invalid source accepted")
		}
	}
}
