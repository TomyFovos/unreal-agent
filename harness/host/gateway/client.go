package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/authflow"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type Client struct {
	http      *http.Client
	transport *http.Transport
}

func NewClient(socket string) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, MaxConnsPerHost: 8}
	return &Client{http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport: transport}
}
func (c *Client) Close() error { c.transport.CloseIdleConnections(); return nil }
func (c *Client) connect(ctx context.Context, path string, q request) (*http.Response, *bufio.Scanner, error) {
	body, err := json.Marshal(q)
	if err != nil {
		return nil, nil, err
	}
	defer clear(body)
	if len(body) > MaxRequestBytes {
		return nil, nil, &Error{Code: "request_too_large"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, &Error{Code: "disconnected"}
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		return nil, nil, &Error{Code: "transport_failed"}
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	return response, scanner, nil
}
func next(scanner *bufio.Scanner) (frame, error) {
	var f frame
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return f, &Error{Code: "invalid_frame"}
		}
		return f, io.EOF
	}
	if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
		return f, &Error{Code: "invalid_frame"}
	}
	if f.Error != "" {
		return f, &Error{Code: f.Error}
	}
	return f, nil
}
func (c *Client) call(ctx context.Context, path string, q request) (frame, error) {
	response, scanner, err := c.connect(ctx, path, q)
	if err != nil {
		return frame{}, err
	}
	defer response.Body.Close()
	return next(scanner)
}
func (c *Client) Open(ctx context.Context, mode host.Mode, id session.ID) (host.View, error) {
	f, e := c.call(ctx, "open", request{ID: id, Mode: mode})
	if e == nil && f.View == nil {
		e = errors.New("gateway: missing view")
	}
	if e != nil {
		return host.View{}, e
	}
	return *f.View, nil
}
func (c *Client) Inspect(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	f, e := c.call(ctx, "inspect", request{ID: id, After: after, Limit: limit})
	if e == nil && f.View == nil {
		e = errors.New("gateway: missing view")
	}
	if e != nil {
		return host.View{}, e
	}
	return *f.View, nil
}
func (c *Client) Submit(ctx context.Context, id session.ID, generation string, input inbox.Input) (host.Receipt, error) {
	f, e := c.call(ctx, "submit", request{ID: id, Generation: generation, Input: input})
	if e == nil && f.Receipt == nil {
		e = errors.New("gateway: missing receipt")
	}
	if e != nil {
		return host.Receipt{}, e
	}
	return *f.Receipt, nil
}
func (c *Client) Stop(ctx context.Context, id session.ID, generation string, mode inbox.ControlMode, reason string) (host.Receipt, error) {
	if mode != inbox.StopHard && mode != inbox.StopWhenIdle {
		return host.Receipt{}, errors.New("gateway: invalid stop mode")
	}
	payload, _ := json.Marshal(inbox.ControlMessage{Mode: mode, Reason: reason})
	return c.Submit(ctx, id, generation, inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload})
}
func (c *Client) Subscribe(ctx context.Context, id session.ID, after sessionstore.Sequence, limit, capacity int) (host.Subscription, error) {
	child, cancel := context.WithCancel(ctx)
	response, scanner, err := c.connect(child, "subscribe", request{ID: id, After: after, Limit: limit, Capacity: capacity})
	if err != nil {
		cancel()
		return host.Subscription{}, err
	}
	first, err := next(scanner)
	if err != nil || first.View == nil {
		response.Body.Close()
		cancel()
		if err == nil {
			err = errors.New("gateway: missing initial view")
		}
		return host.Subscription{}, err
	}
	events := make(chan host.Event, max(1, min(capacity, 128)))
	var once sync.Once
	closeStream := func() { once.Do(func() { cancel(); response.Body.Close() }) }
	go func() {
		running := first.View.Running
		defer close(events)
		defer closeStream()
		for {
			f, e := next(scanner)
			if e != nil {
				if child.Err() == nil && running {
					select {
					case events <- host.Event{Generation: first.View.Generation, Kind: "disconnected"}:
					case <-child.Done():
					}
				}
				return
			}
			if f.Event == nil {
				return
			}
			if f.Event.Kind == "stopped" {
				running = false
			}
			select {
			case events <- *f.Event:
			case <-child.Done():
				return
			}
		}
	}()
	return host.Subscription{Initial: *first.View, Events: events, Cancel: closeStream}, nil
}
func (c *Client) Methods(ctx context.Context) ([]authflow.Support, error) {
	f, e := c.call(ctx, "auth/methods", request{})
	return f.Methods, e
}
func (c *Client) Login(ctx context.Context, reference credential.Reference, secret credential.Secret) (credential.Metadata, error) {
	f, e := c.call(ctx, "auth/login", request{Reference: reference, Secret: secret.Reveal()})
	if e != nil {
		return credential.Metadata{}, e
	}
	if f.Metadata == nil {
		return credential.Metadata{}, errors.New("gateway: missing metadata")
	}
	return *f.Metadata, nil
}
func (c *Client) Logout(ctx context.Context, reference credential.Reference) error {
	_, e := c.call(ctx, "auth/logout", request{Reference: reference})
	return e
}
func (c *Client) ListCredentials(ctx context.Context) ([]credential.Metadata, error) {
	f, e := c.call(ctx, "auth/list", request{})
	return f.Entries, e
}

// Extension carries one bounded JSON value to the owner-installed extension.
// That extension defines its own typed protocol; this transport neither reads
// Session files nor grants access to arbitrary child owners.
func (c *Client) Extension(ctx context.Context, value jsontext.Value) (jsontext.Value, error) {
	if len(value) > MaxRequestBytes || !value.IsValid() {
		return nil, &Error{Code: "invalid_extension_request"}
	}
	body := value.Clone()
	defer clear(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/extension", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Code: "disconnected"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &Error{Code: "extension_failed"}
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, MaxFrameBytes+1))
	if err != nil {
		return nil, &Error{Code: "invalid_extension_frame"}
	}
	if len(result) > MaxFrameBytes || !jsontext.Value(result).IsValid() {
		return nil, &Error{Code: "invalid_extension_frame"}
	}
	return result, nil
}
