package viewer

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestViewPreferenceRoundTrip(t *testing.T) {
	directory := t.TempDir()
	handler := ExtensionHandler{Directory: directory}
	remote := Remote{ParentID: "fx", Call: func(ctx context.Context, body jsontext.Value) (jsontext.Value, error) {
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/", bytes.NewReader(body))
		request.Host = "localhost"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Body.Bytes(), nil
	}}
	if got := remote.SavedView(t.Context()); got != "" {
		t.Fatalf("no saved view yet, got %q", got)
	}
	for _, view := range []string{"chat", "orchestration", "split"} {
		if err := remote.SaveView(t.Context(), view); err != nil {
			t.Fatal(view, err)
		}
		if got := remote.SavedView(t.Context()); got != view {
			t.Fatalf("saved %q, read %q", view, got)
		}
	}
	if err := remote.SaveView(t.Context(), "wide"); err == nil {
		t.Fatal("an unknown view was saved")
	}
	info, err := os.Stat(filepath.Join(directory, "fx.view"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("view file", info, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "fx.view"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := remote.SavedView(t.Context()); got != "" {
		t.Fatalf("a corrupt file must read as no preference, got %q", got)
	}
	bad := Remote{ParentID: "../escape", Call: remote.Call}
	if err := bad.SaveView(t.Context(), "chat"); err == nil {
		t.Fatal("a path-like session name was accepted")
	}
}
