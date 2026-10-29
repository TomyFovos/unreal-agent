package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func TestSessionFailureReachesDiagnosticObserverWithOriginalCause(t *testing.T) {
	cause := errors.New("provider: unauthenticated stage=http_status status=401")
	logged := make(chan error, 1)
	h, err := New(t.Context(), Config{Directory: t.TempDir(), Build: factory(modelFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, cause
	})), OnFailure: func(id session.ID, err error) {
		if id != "diagnostic" {
			t.Error("wrong session", id)
		}
		logged <- err
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := h.Create(t.Context(), Options{ID: "diagnostic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Submit(timeout(t), s.Generation, input("request", "hello")); err != nil {
		t.Fatal(err)
	}
	if err = s.Wait(timeout(t)); !errors.Is(err, cause) {
		t.Fatal("failure cause lost", err)
	}
	select {
	case err := <-logged:
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "status=401") {
			t.Fatal(err)
		}
	default:
		t.Fatal("failure was not logged before session stopped")
	}
}
