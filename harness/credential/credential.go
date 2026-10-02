// Package credential defines secret-free references and request-time authentication.
package credential

import (
	"context"
	"encoding/json/v2"
	"errors"
	"regexp"
	"time"
)

type Method string

const (
	None   Method = "none"
	APIKey Method = "api_key"
	OAuth  Method = "oauth"
)

type Owner string

const (
	Managed  Owner = "harness"
	External Owner = "external"
)

// Reference may be persisted in a session. It never contains credential material.
type Reference struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Method   Method `json:"method"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func (r Reference) Validate() error {
	if !identifier.MatchString(r.Provider) || !identifier.MatchString(r.ID) || (r.Method != APIKey && r.Method != OAuth) {
		return &Error{Code: "invalid_reference"}
	}
	return nil
}

// Secret deliberately redacts JSON and all common fmt formatting. Reveal is only for
// credential backends and provider request construction. Never log its result.
type Secret struct{ value string }

func NewSecret(value string) Secret           { return Secret{value: value} }
func (s Secret) Reveal() string               { return s.value }
func (s Secret) String() string               { return "[redacted]" }
func (s Secret) GoString() string             { return "credential.Secret{[redacted]}" }
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal("[redacted]") }

type Material struct {
	Token        Secret
	RefreshToken Secret
	AccountID    string
	ExpiresAt    time.Time
	Owner        Owner
}
type Resolver interface {
	Resolve(context.Context, Reference) (Material, error)
}
type ResolverFunc func(context.Context, Reference) (Material, error)

func (f ResolverFunc) Resolve(ctx context.Context, r Reference) (Material, error) { return f(ctx, r) }

// Error intentionally has no cause/string supplied by a provider: upstream errors
// and token endpoint response bodies may contain secrets.
type Error struct{ Code string }

func (e *Error) Error() string           { return "credential: " + e.Code }
func IsCode(err error, code string) bool { var e *Error; return errors.As(err, &e) && e.Code == code }
