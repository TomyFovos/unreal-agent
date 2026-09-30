package viewer

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"net/http"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

const maxExtensionRequest = 128 << 10
const maxExtensionReply = 32 << 20

type extensionRequest struct {
	Action   string
	ParentID session.ID
	ID       session.ID
	After    sessionstore.Sequence
	Limit    int
	Control  ControlRequest
}
type extensionReply struct {
	View    *host.View    `json:",omitzero"`
	Receipt *host.Receipt `json:",omitzero"`
	Error   string        `json:",omitzero"`
}
type ExtensionError struct{ Code string }

func (e *ExtensionError) Error() string { return "viewer: " + e.Code }
func (e *ExtensionError) Is(target error) bool {
	switch e.Code {
	case "unavailable":
		return target == ErrUnavailable
	case "stale_generation":
		return target == host.ErrStaleGeneration
	case "stopped":
		return target == host.ErrStopped
	case "conflict":
		return target == host.ErrConflict
	case "not_found":
		return target == fs.ErrNotExist
	case "invalid_page":
		return target == ErrInvalidPage
	}
	return false
}
func extensionError(err error) string {
	switch {
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, host.ErrStaleGeneration):
		return "stale_generation"
	case errors.Is(err, host.ErrStopped):
		return "stopped"
	case errors.Is(err, host.ErrConflict):
		return "conflict"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	case errors.Is(err, ErrInvalidPage):
		return "invalid_page"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "request_failed"
	}
}

// ExtensionHandler is mounted only on the authenticated private local gateway.
// It revalidates parent/child identity at the owning Host for every action.
type ExtensionHandler struct {
	Host      *host.Host
	Directory string
}

func (h ExtensionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Host != "localhost" || r.Header.Get("Origin") != "" {
		http.Error(w, "invalid local request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxExtensionRequest))
	defer clear(body)
	var q extensionRequest
	if err == nil {
		err = json.Unmarshal(body, &q, json.RejectUnknownMembers(true))
	}
	out := extensionReply{}
	if err != nil {
		out.Error = "invalid_request"
	} else {
		switch q.Action {
		case "inspect":
			v, e := (ParentReader{Host: h.Host, ParentID: q.ParentID, Directory: h.Directory}).Inspect(r.Context(), q.ID, q.After, q.Limit)
			if e != nil {
				err = e
			} else {
				out.View = &v
			}
		case "steer", "cancel", "resume":
			controls := HostControls{Host: h.Host}
			var receipt host.Receipt
			switch q.Action {
			case "steer":
				receipt, err = controls.SteerChild(r.Context(), q.Control)
			case "cancel":
				receipt, err = controls.CancelChild(r.Context(), q.Control)
			case "resume":
				err = controls.ResumeChild(r.Context(), q.Control)
			}
			if err == nil && q.Action != "resume" {
				out.Receipt = &receipt
			}
		default:
			out.Error = "unsupported_action"
		}
		if err != nil {
			out.Error = extensionError(err)
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > maxExtensionReply {
		encoded = []byte(`{"Error":"reply_too_large"}`)
	}
	_, _ = w.Write(encoded)
}

type ExtensionCall func(context.Context, jsontext.Value) (jsontext.Value, error)

// Remote uses a gateway's existing private-socket transport. Parent subscription
// stays on the normal Host API; scoped child reads and controls use Extension.
type Remote struct {
	ParentID session.ID
	Parent   Reader
	Call     ExtensionCall
}

func (r Remote) exchange(ctx context.Context, q extensionRequest) (extensionReply, error) {
	if r.Call == nil {
		return extensionReply{}, ErrUnavailable
	}
	encoded, err := json.Marshal(q)
	if err != nil {
		return extensionReply{}, err
	}
	data, err := r.Call(ctx, encoded)
	if err != nil {
		return extensionReply{}, err
	}
	if len(data) > maxExtensionReply {
		return extensionReply{}, &ExtensionError{Code: "reply_too_large"}
	}
	var reply extensionReply
	if err = json.Unmarshal(data, &reply, json.RejectUnknownMembers(true)); err != nil {
		return reply, &ExtensionError{Code: "invalid_reply"}
	}
	if reply.Error != "" {
		return reply, &ExtensionError{Code: reply.Error}
	}
	return reply, nil
}
func (r Remote) Inspect(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	if id == r.ParentID {
		if r.Parent == nil {
			return host.View{}, ErrUnavailable
		}
		return r.Parent.Inspect(ctx, id, after, limit)
	}
	reply, err := r.exchange(ctx, extensionRequest{Action: "inspect", ParentID: r.ParentID, ID: id, After: after, Limit: limit})
	if err != nil {
		return host.View{}, err
	}
	if reply.View == nil || reply.View.Session.Session.ID != id {
		return host.View{}, &ExtensionError{Code: "invalid_reply"}
	}
	return *reply.View, nil
}
func (r Remote) Subscribe(ctx context.Context, id session.ID, after sessionstore.Sequence, limit, capacity int) (host.Subscription, error) {
	if id == r.ParentID {
		if r.Parent == nil {
			return host.Subscription{}, ErrUnavailable
		}
		return r.Parent.Subscribe(ctx, id, after, limit, capacity)
	}
	if capacity < 1 || capacity > 4096 {
		return host.Subscription{}, ErrInvalidPage
	}
	v, err := r.Inspect(ctx, id, after, limit)
	if err != nil {
		return host.Subscription{}, err
	}
	closed := make(chan host.Event)
	close(closed)
	return host.Subscription{Initial: v, Events: closed, Cancel: func() {}}, nil
}
func (r Remote) receipt(ctx context.Context, action string, q ControlRequest) (host.Receipt, error) {
	if q.ParentID != r.ParentID {
		return host.Receipt{}, ErrUnavailable
	}
	reply, err := r.exchange(ctx, extensionRequest{Action: action, Control: q})
	if err != nil {
		return host.Receipt{}, err
	}
	if reply.Receipt == nil || reply.Receipt.ID != q.InputID {
		return host.Receipt{}, &ExtensionError{Code: "invalid_reply"}
	}
	return *reply.Receipt, nil
}
func (r Remote) SteerChild(ctx context.Context, q ControlRequest) (host.Receipt, error) {
	return r.receipt(ctx, "steer", q)
}
func (r Remote) CancelChild(ctx context.Context, q ControlRequest) (host.Receipt, error) {
	return r.receipt(ctx, "cancel", q)
}
func (r Remote) ResumeChild(ctx context.Context, q ControlRequest) error {
	if q.ParentID != r.ParentID {
		return ErrUnavailable
	}
	_, err := r.exchange(ctx, extensionRequest{Action: "resume", Control: q})
	return err
}

var _ Reader = Remote{}
var _ Controls = Remote{}
