package responsesapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestProgressFromSSEDoesNotChangeCanonicalResponse(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		attempt := attempts.Add(1)
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial-%d\"}\n\n", attempt)
		if attempt == 1 {
			return
		}
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", completedResponse)
	}))
	defer server.Close()
	adapter := newTestAdapterWithConfig(t, Config{Endpoint: server.URL, MaxAttempts: new(2)})
	var progress []llm.Progress
	response, err := adapter.Respond(t.Context(), validRequest(), llm.RequestOptions{Progress: func(p llm.Progress) { progress = append(progress, p) }})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := decodeResponse([]byte(completedResponse))
	if !reflect.DeepEqual(response, want) {
		t.Fatal("partial output entered canonical response")
	}
	if len(progress) != 4 || !progress[0].Reset || progress[1].Delta != "partial-1" || !progress[2].Reset || progress[2].Attempt != 2 || progress[3].Delta != "partial-2" {
		t.Fatalf("progress: %+v", progress)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	emitProgress(context.WithValue(ctx, progressKey{}, func(llm.Progress) { called = true }), llm.Progress{})
	if called {
		t.Fatal("canceled callback")
	}
}
