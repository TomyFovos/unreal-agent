package openaicodex

import (
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestCodexPermissionDenialDoesNotRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client, err := NewClient(Config{
			AccessToken: "synthetic-token", AccountID: "synthetic-account",
			HTTPClient: &http.Client{Transport: sandboxTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("unexpected network request")
			})},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		started := time.Now()
		_, err = client.Respond(permission.WithPolicy(t.Context(), permission.DenyAll()), llm.Request{}, llm.RequestOptions{})
		denial := permission.Failure(err)
		diagnostic, ok := responsesapi.Diagnose(err)
		if denial == nil || denial.Code != permission.Denied || !ok || diagnostic.Retryable || calls != 0 || time.Since(started) != 0 {
			t.Fatalf("error=%v diagnostic=%+v calls=%d elapsed=%v", err, diagnostic, calls, time.Since(started))
		}
	})
}
