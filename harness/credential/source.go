package credential

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Source contains only an explicitly selected location, never a key value.
// An empty source does not discover conventional API key environment variables.
type Source struct {
	File string `json:"file,omitzero"`
	Env  string `json:"env,omitzero"`
}

var environmentName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

const ExplicitAPIKeyID = "explicit-api-key"

func (s Source) Validate() error {
	if (s.File == "") == (s.Env == "") || s.File != "" && !filepath.IsAbs(s.File) || s.Env != "" && !environmentName.MatchString(s.Env) {
		return &Error{Code: "invalid_api_key_source"}
	}
	return nil
}

func (s Source) Read(ctx context.Context, getenv func(string) string) (Secret, error) {
	if err := ctx.Err(); err != nil {
		return Secret{}, err
	}
	if err := s.Validate(); err != nil {
		return Secret{}, err
	}
	var value string
	if s.File != "" {
		f, err := os.Open(s.File)
		if err != nil {
			return Secret{}, &Error{Code: "api_key_file_unreadable"}
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return Secret{}, &Error{Code: "api_key_file_unreadable"}
		}
		if info.Mode().Perm() != 0o600 {
			return Secret{}, &Error{Code: "api_key_file_requires_0600"}
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return Secret{}, &Error{Code: "api_key_file_unreadable"}
		}
		value = strings.TrimSpace(string(data))
		clear(data)
	} else {
		if getenv == nil {
			getenv = os.Getenv
		}
		value = strings.TrimSpace(getenv(s.Env))
	}
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return Secret{}, &Error{Code: "api_key_missing_or_invalid"}
	}
	return NewSecret(value), nil
}

// WithSources resolves only operator locations registered for an explicit API
// key reference. Moving a file does not change the Session's reference identity.
// Managed credentials and subscription authentication retain their own resolver.
func WithSources(base Resolver, sources map[string]Source, getenv func(string) string) Resolver {
	return ResolverFunc(func(ctx context.Context, ref Reference) (Material, error) {
		if err := ref.Validate(); err != nil {
			return Material{}, err
		}
		if ref.Method == APIKey && ref.ID == ExplicitAPIKeyID {
			source, ok := sources[ref.Provider]
			if !ok {
				return Material{}, &Error{Code: "api_key_source_unconfigured"}
			}
			key, err := source.Read(ctx, getenv)
			return Material{Token: key, Owner: External, Source: source}, err
		}
		if base == nil {
			return Material{}, &Error{Code: "store_required"}
		}
		return base.Resolve(ctx, ref)
	})
}
