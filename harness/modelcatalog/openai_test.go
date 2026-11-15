package modelcatalog

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestOpenAIAPIModelCatalog(t *testing.T) {
	key := credential.NewSecret("fake-catalog-key")
	status := 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+key.Reveal() {
			t.Error("wrong API model request")
		}
		w.WriteHeader(status)
		if status != 200 {
			fmt.Fprint(w, key.Reveal())
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-b"},{"id":"gpt-a"},{"id":"gpt-b"},{"id":"fake-catalog-key"}]}`)
	}))
	defer server.Close()
	c, err := OpenAI(t.Context(), server.URL+"/v1", key, server.Client())
	if err != nil || !c.Authoritative || len(c.Models) != 2 || c.Models[0].ID != "gpt-a" || !c.Models[0].AllowsEffort("") {
		t.Fatal("invalid API catalog normalization")
	}
	for _, effort := range []llm.ReasoningEffort{"low", "medium", "high", "xhigh", "max"} {
		if !c.Models[0].AllowsEffort(effort) {
			t.Fatalf("explicit API effort %s rejected before reaching API", effort)
		}
	}
	for _, code := range []int{401, 403, 429, 500} {
		status = code
		_, err = OpenAI(t.Context(), server.URL+"/v1", key, server.Client())
		if err == nil || strings.Contains(err.Error(), key.Reveal()) {
			t.Fatal("unsafe API catalog failure")
		}
	}
}

func TestOpenAIAPICatalogDoesNotForwardKeyOnRedirect(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer server.Close()
	if _, err := OpenAI(t.Context(), server.URL, credential.NewSecret("fake-redirect-key"), nil); err == nil || calls != 0 {
		t.Fatal("API catalog followed redirect")
	}
}
