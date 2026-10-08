package claudecode

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// SDKAssistantMessageError from the locally pinned 0.3.285 sdk.d.ts. Store only
// the enum, never the provider-supplied string/body. Zero means no SDK error;
// an unrecognized string is distinct from the SDK's literal "unknown".
type assistantErrorKind uint8

const (
	assistantErrorNone assistantErrorKind = iota
	assistantErrorUnrecognized
	assistantAuthenticationFailed
	assistantOAuthOrgNotAllowed
	assistantAccountOnHold
	assistantVerificationRequired
	assistantBillingError
	assistantRateLimit
	assistantOverloaded
	assistantInvalidRequest
	assistantModelNotFound
	assistantServerError
	assistantUnknown
	assistantMaxOutputTokens
	assistantCloudCredentialError
)

func parseAssistantError(value string) assistantErrorKind {
	switch value {
	case "authentication_failed":
		return assistantAuthenticationFailed
	case "oauth_org_not_allowed":
		return assistantOAuthOrgNotAllowed
	case "account_on_hold":
		return assistantAccountOnHold
	case "verification_required":
		return assistantVerificationRequired
	case "billing_error":
		return assistantBillingError
	case "rate_limit":
		return assistantRateLimit
	case "overloaded":
		return assistantOverloaded
	case "invalid_request":
		return assistantInvalidRequest
	case "model_not_found":
		return assistantModelNotFound
	case "server_error":
		return assistantServerError
	case "unknown":
		return assistantUnknown
	case "max_output_tokens":
		return assistantMaxOutputTokens
	case "cloud_credential_error":
		return assistantCloudCredentialError
	default:
		return assistantErrorUnrecognized
	}
}

func (e *Error) AssistantReason() string {
	switch e.assistantError {
	case assistantErrorUnrecognized:
		return "assistant_provider_error_unknown"
	case assistantAuthenticationFailed:
		return "assistant_authentication_failed"
	case assistantOAuthOrgNotAllowed:
		return "assistant_oauth_org_not_allowed"
	case assistantAccountOnHold:
		return "assistant_account_on_hold"
	case assistantVerificationRequired:
		return "assistant_verification_required"
	case assistantBillingError:
		return "assistant_billing_error"
	case assistantRateLimit:
		return "assistant_rate_limit"
	case assistantOverloaded:
		return "assistant_overloaded"
	case assistantInvalidRequest:
		return "assistant_invalid_request"
	case assistantModelNotFound:
		return "assistant_model_not_found"
	case assistantServerError:
		return "assistant_server_error"
	case assistantUnknown:
		return "assistant_unknown"
	case assistantMaxOutputTokens:
		return "assistant_max_output_tokens"
	case assistantCloudCredentialError:
		return "assistant_cloud_credential_error"
	default:
		return ""
	}
}

func assistantFailure(raw jsontext.Value) *Error {
	var value string
	if raw.Kind() != '"' || json.Unmarshal(raw, &value) != nil {
		return &Error{Code: "malformed_stream", assistantError: assistantErrorUnrecognized}
	}
	kind := parseAssistantError(value)
	code := "subprocess_failure"
	switch kind {
	case assistantAuthenticationFailed, assistantCloudCredentialError:
		// Authentication stays with the external CLI. Do not import credentials
		// or switch billing/routing when its credential resolution fails.
		code = "external_reauth_required"
	case assistantOAuthOrgNotAllowed, assistantAccountOnHold, assistantVerificationRequired, assistantBillingError:
		code = "subscription_unavailable"
	case assistantRateLimit:
		code = "rate_limited"
	case assistantModelNotFound:
		code = "invalid_model"
	case assistantInvalidRequest:
		code = "provider_request_rejected"
	case assistantMaxOutputTokens:
		code = "generation_output_limit"
	}
	return &Error{Code: code, assistantError: kind}
}
