package sessionstore

import "testing"

func TestKnownModelHTTPFailureRecordRejectsFabricatedSuccessAndOtherKinds(t *testing.T) {
	for _, status := range []int{0, 200, 299, 600} {
		r := HostRecord{Version: 1, Kind: HostModelRequestFailure, ModelRequestFailure: &ModelRequestFailure{TurnID: "turn", HTTPStatus: status}}
		if r.Validate() == nil {
			t.Fatal("invalid known HTTP failure accepted", status)
		}
	}
	r := HostRecord{Version: 1, Kind: HostModelRequestFailure, ModelRequestFailure: &ModelRequestFailure{TurnID: "turn", HTTPStatus: 503}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	r.Kind = "configuration"
	if r.Validate() == nil {
		t.Fatal("request failure attached to another canonical record")
	}
}
