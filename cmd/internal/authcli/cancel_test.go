package authcli

import (
	"context"
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLoginCancelsBlockedPrivatePipe(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	privateInput := &notifyingInput{ReadCloser: reader, entered: entered}
	directory := filepath.Join(t.TempDir(), "store")
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, []string{"login", "--store", directory, "--provider", "openai", "--id", "work", "--key-stdin"}, privateInput, io.Discard)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("login never attempted private input")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("login did not cancel")
	}
}
func TestUnsupportedLoginDoesNotReadSecret(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := Run(ctx, []string{"login", "--store", filepath.Join(t.TempDir(), "store"), "--provider", "anthropic", "--id", "work", "--method", "oauth", "--key-stdin"}, reader, io.Discard)
	if !credential.IsCode(err, "unsupported_auth_flow") {
		t.Fatal("unsupported method asked for a secret", err)
	}
}

type notifyingInput struct {
	io.ReadCloser
	entered chan struct{}
	once    sync.Once
}

func (r *notifyingInput) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.ReadCloser.Read(p)
}
