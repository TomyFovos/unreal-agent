package provider

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients/claudeagent"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

func TestNormalizeRetainsClosedToolIdentityErrors(t *testing.T) {
	for _, sentinel := range []error{llm.ErrToolIdentityConflict, llm.ErrToolRecoveryRequired} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			for _, input := range []error{sentinel, fmt.Errorf("synthetic-private-wrapper: %w", sentinel), errors.Join(errors.New("synthetic-private-cause"), sentinel)} {
				got := normalize(input)
				if got != sentinel || !errors.Is(got, sentinel) || errors.Unwrap(got) != nil {
					t.Fatalf("closed identity error lost or raw wrapper retained: %v", got)
				}
				data, err := json.Marshal(struct{ Error string }{Error: got.Error()})
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data)+fmt.Sprintf("%v %+v %#v", got, got, got), "synthetic-private") {
					t.Fatal("identity error retained private cause")
				}
			}
			got := normalize(errors.New(sentinel.Error()))
			var generic *Error
			if errors.Is(got, sentinel) || !errors.As(got, &generic) || generic.Code != "request_failed" {
				t.Fatal("error text was treated as a typed sentinel")
			}
		})
	}
}

func TestToolIdentityNormalizationPreservesExistingPrecedence(t *testing.T) {
	for _, sentinel := range []error{llm.ErrToolIdentityConflict, llm.ErrToolRecoveryRequired} {
		for _, primary := range []error{
			context.Canceled, context.DeadlineExceeded,
			&permission.Error{Code: permission.Denied, Capability: "file.write", Reason: "not allowed"},
			&credential.Error{Code: "external_reauth_required"},
			&claudeagent.Error{Code: "external_reauth_required"},
			&Error{Code: "unsupported_model"},
			&responsesapi.APIError{StatusCode: 401, Message: "synthetic-private-body"},
			&responsesapi.APIError{StatusCode: 403, Message: "synthetic-private-body"},
		} {
			want, got := normalize(primary), normalize(errors.Join(primary, sentinel))
			if got.Error() != want.Error() || errors.Is(got, sentinel) {
				t.Fatalf("existing typed error precedence changed: %v / %v", want, got)
			}
		}
	}
	if got := normalize(errors.Join(llm.ErrToolIdentityConflict, llm.ErrToolRecoveryRequired)); got != llm.ErrToolIdentityConflict {
		t.Fatal("ambiguous identity/recovery failure did not prefer conflict")
	}
}

type identityFailureClient struct {
	cause    error
	responds *int
	closes   *int
}

func (c *identityFailureClient) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	*c.responds++
	return llm.Response{}, c.cause
}
func (c *identityFailureClient) Close() error { *c.closes++; return nil }

func TestRegistryRespondPreservesToolIdentityErrors(t *testing.T) {
	for _, stage := range []string{"factory", "respond"} {
		for _, sentinel := range []error{llm.ErrToolIdentityConflict, llm.ErrToolRecoveryRequired} {
			t.Run(stage+"/"+sentinel.Error(), func(t *testing.T) {
				factories, responds, closes := 0, 0, 0
				registry, err := New(Descriptor{ID: "synthetic", AuthMethods: []credential.Method{credential.None}, Models: []Model{{ID: "fixture"}}, New: func(BuildConfig, credential.Material) (Client, error) {
					factories++
					cause := fmt.Errorf("synthetic-private-wrapper: %w", sentinel)
					if stage == "factory" {
						return nil, cause
					}
					return &identityFailureClient{cause: cause, responds: &responds, closes: &closes}, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				client, _, err := registry.Build(BuildConfig{Selection: Selection{Version: 1, Provider: "synthetic", Model: Model{ID: "fixture"}, Endpoint: "http://127.0.0.1", Auth: credential.Reference{Method: credential.None}, MaxAttempts: 1, Source: "synthetic fixture"}})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				for range 2 {
					response, failure := client.Respond(t.Context(), llm.Request{Model: llm.Model{ID: "fixture"}}, llm.RequestOptions{Tools: func(context.Context, llm.Response) ([]llm.ToolOutcome, error) {
						t.Fatal("provider failure invoked execution callback")
						return nil, nil
					}})
					if failure != sentinel || !errors.Is(failure, sentinel) || len(response.Output) != 0 {
						t.Fatalf("Registry Build/Respond lost identity error: %v", failure)
					}
				}
				if factories != 2 || stage == "respond" && (responds != 2 || closes != 2) || stage == "factory" && (responds != 0 || closes != 0) {
					t.Fatalf("request lifetime changed: factories=%d responds=%d closes=%d", factories, responds, closes)
				}
			})
		}
	}
}
