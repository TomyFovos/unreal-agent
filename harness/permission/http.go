package permission

import (
	"net/http"
)

// NetworkUnrestricted reports whether every intersected policy permits all
// HTTP(S) origins. It does not grant any process execution capability.
func (p *Policy) NetworkUnrestricted() bool {
	return p.each(func(p *Policy) error {
		if p.all || p.config.NetworkUnrestricted {
			return nil
		}
		return deny("network", "restricted")
	}) == nil
}

// RoundTripper wraps each HTTP exchange, including redirects. A custom base is
// a trusted host implementation. With nil base, restricted policies use a
// standard transport with ambient proxies disabled to avoid an extra egress
// destination that the origin allowlist did not authorize.
func (p *Policy) RoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if !p.NetworkUnrestricted() {
			transport.Proxy = nil
		}
		base = transport
	}
	return &transport{policy: p, base: base}
}

type transport struct {
	policy *Policy
	base   http.RoundTripper
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.policy.CheckURL(request.URL); err != nil {
		return nil, err
	}
	if err := FromContext(request.Context()).CheckURL(request.URL); err != nil {
		return nil, err
	}
	response, err := t.base.RoundTrip(request)
	if failure := Failure(err); failure != nil {
		return response, failure
	}
	return response, err
}
func (t *transport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
