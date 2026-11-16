package responsesapi

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

func TestAdapterPreservesTypedPermissionRequestFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	synctest.Test(t, func(t *testing.T) {
		remote := primitives.NewRemoteClient()
		defer remote.Close()
		adapter, err := NewAdapter(remote, Config{Endpoint: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, err = adapter.Respond(permission.WithPolicy(t.Context(), permission.DenyAll()), llm.Request{Model: llm.Model{ID: "test"}, Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "hello"}}}}, llm.RequestOptions{})
		denial := permission.Failure(err)
		if denial == nil || denial.Code != permission.Denied || calls.Load() != 0 {
			t.Fatalf("failure=%v calls=%d", err, calls.Load())
		}
		if elapsed := time.Since(started); elapsed != 0 {
			t.Fatalf("permission denial waited for a retry/backoff timer: %v", elapsed)
		}
		if diagnostic, ok := Diagnose(err); !ok || diagnostic.Retryable {
			t.Fatalf("permission denial is retryable: %+v, %v", diagnostic, err)
		}
	})
}
