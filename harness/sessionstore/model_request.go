package sessionstore

import (
	"errors"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

const HostModelRequestFailure = "model_request_failure"

// ModelRequestFailure closes only the replay uncertainty of a canonical model
// attempt that received a definite HTTP failure. It is not a successful model
// response, delivered input, execution receipt or actual-accounting assertion.
// Transport failures and partial/malformed bodies cannot create this record.
type ModelRequestFailure struct {
	TurnID     session.TurnID
	HTTPStatus int
}

func (f ModelRequestFailure) Validate() error {
	if !conversationIdentity(string(f.TurnID)) || f.HTTPStatus < 300 || f.HTTPStatus > 599 {
		return errors.New("invalid known model HTTP failure")
	}
	return nil
}
