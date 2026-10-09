package gateway

import (
	"context"
	"encoding/json/v2"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"io"
	"net/http"
	"sync"
)

func servePermissions(w http.ResponseWriter, r *http.Request, c Config) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	if !authenticated(r.Context()) {
		writeFrame(w, frame{Error: "unauthenticated_control"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBytes))
	defer clear(body)
	var q request
	if err != nil || json.Unmarshal(body, &q, json.RejectUnknownMembers(true)) != nil || q.Permission == nil {
		writeFrame(w, frame{Error: "invalid_permission_control"})
		return
	}
	s, err := c.Host.Attach(q.ID)
	if err != nil {
		writeFrame(w, frame{Error: errorCode(err)})
		return
	}
	control, err := s.PermissionControl(q.Generation)
	if err != nil {
		writeFrame(w, frame{Error: "permission_control_unavailable"})
		return
	}
	p := q.Permission
	if !validPermissionRequest(*p) {
		writeFrame(w, frame{Error: "invalid_permission_control"})
		return
	}
	var reply permission.ControlReply
	switch p.Action {
	case "watch":
		if p.Lease != "" || p.Challenge != "" || p.Mode != "" || p.Operation != "" || p.Allow || p.Expected != 0 {
			writeFrame(w, frame{Error: "invalid_permission_control"})
			return
		}
		key, release := control.AcquireControl()
		defer release()
		for {
			snapshot, changed := control.Watch()
			reply = permission.ControlReply{Snapshot: snapshot, Lease: key}
			if writeFrame(w, frame{Permission: &reply}) != nil {
				return
			}
			key = "" // authority is sent once, never repeated in projections
			select {
			case <-r.Context().Done():
				return
			case <-s.Done():
				return
			case <-changed:
			}
		}
	case "prepare_full":
		reply.Challenge, err = control.PrepareFull(p.Lease, p.Expected)
	case "set_mode":
		err = control.SetMode(p.Lease, p.Expected, p.Mode, p.Challenge)
	case "decide":
		err = control.Decide(p.Lease, p.Expected, p.Operation, p.Allow)
	default:
		err = permission.ErrControl
	}
	if err != nil {
		writeFrame(w, frame{Error: "stale_permission_control"})
		return
	}
	reply.Snapshot = control.Snapshot()
	writeFrame(w, frame{Permission: &reply})
}

func validPermissionRequest(p permission.ControlRequest) bool {
	if p.Action == "watch" {
		return p.Lease == "" && p.Expected == 0 && p.Mode == "" && p.Challenge == "" && p.Operation == "" && !p.Allow
	}
	if len(p.Lease) != 64 || p.Expected == 0 {
		return false
	}
	switch p.Action {
	case "prepare_full":
		return p.Mode == "" && p.Challenge == "" && p.Operation == "" && !p.Allow
	case "set_mode":
		return p.Mode.Valid() && p.Operation == "" && !p.Allow && (p.Mode == permission.FullAccess && len(p.Challenge) == 64 || p.Mode != permission.FullAccess && p.Challenge == "")
	case "decide":
		return p.Mode == "" && p.Challenge == "" && p.Operation != "" && len(p.Operation) <= 256
	}
	return false
}

// PermissionWatch holds a private local control lease only for this attachment.
// Dropping it invalidates replies and refuses pending unapproved operations;
// already approved Full Access remains valid within the same Host Session.
func (c *Client) WatchPermissions(ctx context.Context, id session.ID, generation string) (permission.ControlConnection, error) {
	child, cancel := context.WithCancel(ctx)
	response, scanner, err := c.connect(child, "permissions", request{ID: id, Generation: generation, Permission: &permission.ControlRequest{Action: "watch"}})
	if err != nil {
		cancel()
		return permission.ControlConnection{}, err
	}
	first, err := next(scanner)
	if err != nil || first.Permission == nil || first.Permission.Lease == "" {
		response.Body.Close()
		cancel()
		if err == nil {
			err = &Error{Code: "invalid_permission_control"}
		}
		return permission.ControlConnection{}, err
	}
	lease := first.Permission.Lease
	updates := make(chan permission.ModeSnapshot, 1)
	var once sync.Once
	closeStream := func() { once.Do(func() { cancel(); response.Body.Close() }) }
	go func() {
		defer close(updates)
		defer closeStream()
		for {
			f, e := next(scanner)
			if e != nil || f.Permission == nil {
				return
			}
			select {
			case updates <- f.Permission.Snapshot:
			case <-child.Done():
				return
			}
		}
	}()
	return permission.ControlConnection{Initial: first.Permission.Snapshot, Updates: updates, Cancel: closeStream, Request: func(ctx context.Context, p permission.ControlRequest) (permission.ControlReply, error) {
		if child.Err() != nil {
			return permission.ControlReply{}, &Error{Code: "permission_control_disconnected"}
		}
		p.Lease = lease
		f, e := c.call(ctx, "permissions", request{ID: id, Generation: generation, Permission: &p})
		if e != nil {
			return permission.ControlReply{}, e
		}
		if f.Permission == nil {
			return permission.ControlReply{}, &Error{Code: "invalid_permission_control"}
		}
		return *f.Permission, nil
	}}, nil
}
