package responsesapi

import (
	"errors"
	"net/http"

	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

// FailureDiagnostic contains only closed categories and an HTTP status. It is
// safe to retain at the public provider boundary; it contains no error body,
// URL, headers, request IDs, arguments or wrapped transport error.
// Retryable describes the existing model-request retry policy, never permission
// to replay an Operation. It does not initiate a retry.
type FailureDiagnostic struct {
	Stage     string `json:"stage"`
	Status    int    `json:"status,omitempty"`
	Reason    string `json:"reason"`
	Retryable bool   `json:"retryable"`
}

func (d FailureDiagnostic) Safe() FailureDiagnostic {
	switch d.Stage {
	case "request_build", "http_status", "stream_error", "stream_decode", "stream_incomplete", "response_decode", "transport":
	default:
		d.Stage = "unknown"
	}
	if d.Status < 100 || d.Status > 599 {
		d.Status = 0
	}
	d.Reason = safeFailureReason(d.Reason)
	return d
}

func safeFailureReason(value string) string {
	// Exact codes already present in the generated Responses API schema or the
	// existing retry policy. Arbitrary upstream strings are never retained.
	switch value {
	case "bio_policy", "data_residency_mismatch", "empty_image_file", "failed_to_download_image",
		"image_content_policy_violation", "image_file_not_found", "image_file_too_large", "image_parse_error",
		"image_too_large", "image_too_small", "invalid_base64_image", "invalid_image", "invalid_image_format",
		"invalid_image_mode", "invalid_image_url", "invalid_prompt", "rate_limit_exceeded", "server_error",
		"unsupported_image_media_type", "vector_store_timeout", "context_length_exceeded", "context_window_exceeded", "insufficient_quota",
		"usage_not_included", "usage_limit_reached", "credit_balance_exhausted", "billing_hard_limit_reached",
		"cyber_policy", "misalignment_policy_violation", "invalid_api_key", "invalid_token",
		"server_is_overloaded", "slow_down", "authentication_error", "permission_error",
		"timeout", "connection_error", "tls_error", "invalid_response", "incomplete_stream":
		return value
	default:
		return "unknown"
	}
}

type diagnosticError struct {
	diagnostic FailureDiagnostic
	cause      error
}

// Internal clients keep their existing errors.As/errors.Is and error text.
// provider.normalize copies only Diagnose's safe value, dropping this cause.
func (e *diagnosticError) Error() string { return e.cause.Error() }
func (e *diagnosticError) Unwrap() error { return e.cause }

func diagnosed(err error, d FailureDiagnostic) error {
	if err == nil {
		return nil
	}
	var existing *diagnosticError
	if errors.As(err, &existing) {
		if existing.diagnostic.Status == 0 && d.Status >= 100 && d.Status <= 599 {
			copy := existing.diagnostic
			copy.Status = d.Status
			return &diagnosticError{diagnostic: copy.Safe(), cause: err}
		}
		return err
	}
	var api *APIError
	if errors.As(err, &api) {
		d.Reason = safeFailureReason(api.Code)
		if d.Reason == "unknown" {
			d.Reason = safeFailureReason(api.Type)
		}
	}
	return &diagnosticError{diagnostic: d.Safe(), cause: err}
}

// Diagnose returns a fresh, sanitized diagnostic without retaining err.
func Diagnose(err error) (FailureDiagnostic, bool) {
	var wrapped *diagnosticError
	if errors.As(err, &wrapped) {
		return wrapped.diagnostic.Safe(), true
	}
	var api *APIError
	if errors.As(err, &api) {
		stage := "http_status"
		if api.StatusCode >= http.StatusOK && api.StatusCode < http.StatusMultipleChoices {
			stage = "stream_error"
		}
		d := FailureDiagnostic{Stage: stage, Status: api.StatusCode, Reason: safeFailureReason(api.Code), Retryable: retryableResponseError(api, primitives.DefaultRemoteRequest("", "", "").RetryPolicy.RetryableStatusCodes)}
		if d.Reason == "unknown" {
			d.Reason = safeFailureReason(api.Type)
		}
		return d.Safe(), true
	}
	return FailureDiagnostic{}, false
}
