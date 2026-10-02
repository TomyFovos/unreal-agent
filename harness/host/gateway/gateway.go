// Package gateway exposes a Host over a private local Unix socket. Connections
// own subscriptions only: closing a client never cancels the session owner.
package gateway

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

const MaxFrameBytes = 32 << 20
const MaxRequestBytes = 128 << 10

// Config is owned by the serving process. Clients cannot choose an execution
// policy, replace the runtime configuration, or create their own store writer.
type Config struct {
	Host          *host.Host
	Policy        *permission.Policy
	Configuration jsontext.Value
	Auth          *authflow.Service
	// Extension serves separately authorized parent/child inspection/control APIs.
	Extension http.Handler
	// Workspace is where newly created sessions discover AGENTS.md.
	Workspace string
}
type request struct {
	ID              session.ID
	Mode            host.Mode
	Generation      string
	After           sessionstore.Sequence
	Limit, Capacity int
	Input           inbox.Input
	Reference       credential.Reference
	Secret          string `json:",omitzero"`
}
type frame struct {
	View     *host.View            `json:",omitzero"`
	Event    *host.Event           `json:",omitzero"`
	Receipt  *host.Receipt         `json:",omitzero"`
	Metadata *credential.Metadata  `json:",omitzero"`
	Entries  []credential.Metadata `json:",omitzero"`
	Methods  []authflow.Support    `json:",omitzero"`
	Error    string                `json:",omitzero"`
}
type Error struct{ Code string }

func (e *Error) Error() string { return "gateway: " + e.Code }
func (e *Error) Is(target error) bool {
	return (e.Code == "stale_generation" && target == host.ErrStaleGeneration) || (e.Code == "writer_owned" && target == localfile.ErrWriterOwned) || (e.Code == "stopped" && target == host.ErrStopped) || (e.Code == "conflict" && target == host.ErrConflict) || (e.Code == "not_found" && target == fs.ErrNotExist)
}
func errorCode(err error) string {
	// Checked first: a discovery failure may wrap an unrelated fs error.
	var pe *projectinstructions.Error
	if errors.As(err, &pe) {
		return "project_instructions_" + string(pe.Code)
	}
	switch {
	case errors.Is(err, host.ErrStaleGeneration):
		return "stale_generation"
	case errors.Is(err, localfile.ErrWriterOwned):
		return "writer_owned"
	case errors.Is(err, host.ErrStopped):
		return "stopped"
	case errors.Is(err, host.ErrConflict):
		return "conflict"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	var ce *credential.Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return "request_failed"
}

// ListenAndServe holds a distinct socket ownership lock. A stale socket can only
// be removed after acquiring that lock. Host shutdown belongs to the caller.
func ListenAndServe(ctx context.Context, path string, c Config) error {
	if c.Host == nil {
		return errors.New("gateway: host required")
	}
	listener, cleanup, err := listen(path)
	if err != nil {
		return err
	}
	defer cleanup()
	server := &http.Server{Handler: Handler(c), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, ReadTimeout: 10 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	defer close(done)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}
func Handler(c Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Host != "localhost" || r.Header.Get("Origin") != "" {
			http.Error(w, "invalid local request", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/extension" && c.Extension != nil {
			c.Extension.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
		defer clear(body)
		var q request
		if err == nil {
			err = json.Unmarshal(body, &q, json.RejectUnknownMembers(true))
		}
		if err != nil {
			writeFrame(w, frame{Error: "invalid_request"})
			return
		}
		result, sub, err := dispatch(r.Context(), c, r.URL.Path, q)
		q.Secret = ""
		if err != nil {
			writeFrame(w, frame{Error: errorCode(err)})
			return
		}
		if sub != nil {
			defer sub.Cancel()
		}
		if err = writeFrame(w, result); err != nil {
			return
		}
		if sub == nil {
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case event, ok := <-sub.Events:
				if !ok {
					return
				}
				if writeFrame(w, frame{Event: &event}) != nil {
					return
				}
			}
		}
	})
}
func dispatch(ctx context.Context, c Config, path string, q request) (frame, *host.Subscription, error) {
	if path == "/v1/auth/methods" {
		return frame{Methods: authflow.Matrix()}, nil, nil
	}
	if path == "/v1/auth/login" || path == "/v1/auth/logout" || path == "/v1/auth/list" {
		if c.Auth == nil {
			return frame{}, nil, &Error{Code: "auth_unavailable"}
		}
		switch path {
		case "/v1/auth/login":
			m, e := c.Auth.Login(ctx, authflow.LoginRequest{Reference: q.Reference, Secret: credential.NewSecret(q.Secret)})
			return frame{Metadata: &m}, nil, e
		case "/v1/auth/logout":
			return frame{}, nil, c.Auth.Logout(ctx, q.Reference)
		default:
			entries, e := c.Auth.List(ctx)
			return frame{Entries: entries}, nil, e
		}
	}
	if q.Limit == 0 {
		q.Limit = 128
	}
	if q.Capacity == 0 {
		q.Capacity = 32
	}
	if q.Limit < 1 || q.Limit > 256 || q.Capacity < 1 || q.Capacity > 128 {
		return frame{}, nil, &Error{Code: "invalid_bounds"}
	}
	var s *host.Session
	var err error
	if path == "/v1/open" {
		s, err = c.Host.Open(ctx, host.Options{Policy: c.Policy, ID: q.ID, Mode: q.Mode, Configuration: c.Configuration, Workspace: c.Workspace})
	} else {
		s, err = c.Host.Attach(q.ID)
	}
	if err != nil {
		return frame{}, nil, err
	}
	switch path {
	case "/v1/open", "/v1/inspect":
		v, e := s.Inspect(q.After, q.Limit)
		return frame{View: &v}, nil, e
	case "/v1/submit":
		receipt, e := s.Submit(ctx, q.Generation, q.Input)
		return frame{Receipt: &receipt}, nil, e
	case "/v1/subscribe":
		sub, e := s.Subscribe(q.After, q.Limit, q.Capacity)
		return frame{View: &sub.Initial}, &sub, e
	default:
		return frame{}, nil, &Error{Code: "unknown_method"}
	}
}
func writeFrame(w http.ResponseWriter, f frame) error {
	encoded, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(encoded) > MaxFrameBytes {
		encoded = []byte(`{"Error":"frame_too_large"}`)
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err = fmt.Fprintf(w, "%s\n", encoded); err != nil {
		return err
	}
	if err = controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
