package testcodex

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json/v2"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// OAuthProxy models Docker's two egress paths entirely on loopback: a direct
// request sees only the sentinel and fails, while CONNECT reaches the proxy's
// credential-injection path. No real credential or external backend is used.
type OAuthProxy struct {
	Transport      *http.Transport
	Connects       atomic.Int32
	Requests       atomic.Int32
	DirectRequests atomic.Int32
	Status         atomic.Int32
	RequestOK      atomic.Bool
}

const DockerProxyURL = "http://host.docker.internal:3128"

func NewOAuthProxy(t *testing.T, publicSentinel string) *OAuthProxy {
	return newOAuthProxy(t, publicSentinel, nil)
}

// NewOAuthProxyWithHandler supplies only the synthetic model backend. The
// same CONNECT, authority, TLS, sentinel and no-account checks still apply.
func NewOAuthProxyWithHandler(t *testing.T, publicSentinel string, handler http.Handler) *OAuthProxy {
	t.Helper()
	return newOAuthProxy(t, publicSentinel, handler)
}

func newOAuthProxy(t *testing.T, publicSentinel string, handler http.Handler) *OAuthProxy {
	t.Helper()
	fixture := &OAuthProxy{}
	fixture.Status.Store(http.StatusOK)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"chatgpt.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	startTLS := func(handler http.Handler) *httptest.Server {
		server := httptest.NewUnstartedServer(handler)
		server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
		server.StartTLS()
		t.Cleanup(server.Close)
		return server
	}
	direct := startTLS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.DirectRequests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"synthetic sentinel was not substituted"}}`)
	}))
	backend := startTLS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.Requests.Add(1)
		var body struct {
			Model     string `json:"model"`
			Stream    bool   `json:"stream"`
			Store     bool   `json:"store"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		data, readErr := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		ok := readErr == nil && len(data) <= 1<<20 && json.Unmarshal(data, &body) == nil && r.Method == http.MethodPost && r.Host == "chatgpt.com" && r.URL.Path == "/backend-api/codex/responses" && r.Header.Get("Authorization") == "Bearer "+publicSentinel && len(r.Header.Values("ChatGPT-Account-ID")) == 0 && r.Header.Get("Originator") == "unreal-agent" && r.Header.Get("Proxy-Authorization") == "" && r.Header.Get("Cookie") == "" && body.Model == "gpt-6.1-sol" && body.Reasoning.Effort == "medium" && body.Stream && !body.Store
		fixture.RequestOK.Store(ok)
		if !ok {
			t.Error("mock proxy request violated the fixed routing/auth/wire contract")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		status := int(fixture.Status.Load())
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"private-fixture-response","code":"private-fixture-code"}}`)
			return
		}
		if handler != nil {
			r.Body = io.NopCloser(bytes.NewReader(data))
			handler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"dummy\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"dummy-message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"CODEX_PROXY_OK\"}]}]}}\n\n")
	}))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.Connects.Add(1)
		if r.Method != http.MethodConnect || r.Host != "chatgpt.com:443" || r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("unexpected CONNECT authority or authentication")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		upstream, err := net.DialTimeout("tcp", backend.Listener.Addr().String(), time.Second)
		if err != nil {
			t.Error("mock proxy backend unavailable")
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error("mock CONNECT hijack failed")
			return
		}
		defer client.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if buffered.Flush() != nil {
			return
		}
		go func() { _, _ = io.Copy(upstream, buffered) }()
		_, _ = io.Copy(client, upstream)
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(DockerProxyURL)
	fixture.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		var target string
		switch address {
		case "host.docker.internal:3128":
			target = proxy.Listener.Addr().String()
		case "chatgpt.com:443":
			target = direct.Listener.Addr().String()
		default:
			return nil, &net.OpError{Op: "dial", Net: network, Err: context.Canceled}
		}
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, strings.TrimSpace(target))
	}}
	t.Cleanup(fixture.Transport.CloseIdleConnections)
	return fixture
}
