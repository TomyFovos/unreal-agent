package coordinator

import (
	"crypto/sha256"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Disposable, Session-wide evidence derived only from durable ModelResponses.
// No arguments, receipts, execution authority or second persistence live here.
type toolIdentity struct {
	digest [sha256.Size]byte
	turn   session.TurnID
}

type admittedToolCall struct {
	id     string
	digest [sha256.Size]byte
}

func toolFingerprint(call llm.ToolCall) ([sha256.Size]byte, error) {
	if call.CallID == "" || call.Name == "" {
		return [sha256.Size]byte{}, llm.ErrToolIdentityConflict
	}
	raw := strings.Trim(call.Arguments, " \t\r\n")
	if len(raw) == 0 || raw[0] != '{' || !jsontext.Value(raw).IsValid() {
		return [sha256.Size]byte{}, llm.ErrToolIdentityConflict
	}
	// UseNumber preserves integer precision and numeric spelling. Do not use
	// RFC8785's float64 conversion for operation identity. Object key ordering,
	// whitespace and string escapes do not change the fingerprint.
	decoder := jsonv1.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if decoder.Decode(&object) != nil || object == nil {
		return [sha256.Size]byte{}, llm.ErrToolIdentityConflict
	}
	canonical, err := jsonv1.Marshal(object)
	if err != nil {
		return [sha256.Size]byte{}, llm.ErrToolIdentityConflict
	}
	hash := sha256.New()
	hash.Write([]byte(call.Name))
	hash.Write([]byte{0})
	hash.Write(canonical)
	var digest [sha256.Size]byte
	hash.Sum(digest[:0])
	return digest, nil
}

// Pure preflight: the entire response is checked before canonical append or
// any translator/Operation. A failed batch reserves no identities.
func (current *coordinator) checkToolAdmission(response llm.Response) ([]admittedToolCall, error) {
	if current.toolAdmissionErr != nil {
		return nil, current.toolAdmissionErr
	}
	var accepted []admittedToolCall
	var batch map[string][sha256.Size]byte
	for _, item := range response.Output {
		if item.Type != llm.ItemToolCall {
			continue
		}
		call, ok := item.Data.(llm.ToolCall)
		if !ok {
			return nil, llm.ErrToolIdentityConflict
		}
		digest, err := toolFingerprint(call)
		if err != nil {
			return nil, err
		}
		if prior, exists := current.toolIdentities[call.CallID]; exists {
			if prior.digest != digest {
				return nil, llm.ErrToolIdentityConflict
			}
			return nil, llm.ErrToolRecoveryRequired
		}
		if prior, exists := batch[call.CallID]; exists {
			if prior != digest {
				return nil, llm.ErrToolIdentityConflict
			}
			return nil, llm.ErrToolRecoveryRequired
		}
		if batch == nil {
			batch = make(map[string][sha256.Size]byte)
		}
		batch[call.CallID] = digest
		accepted = append(accepted, admittedToolCall{id: call.CallID, digest: digest})
	}
	return accepted, nil
}

func (current *coordinator) rememberToolAdmission(turn session.TurnID, accepted []admittedToolCall) {
	if len(accepted) == 0 {
		return
	}
	if current.toolIdentities == nil {
		current.toolIdentities = make(map[string]toolIdentity)
	}
	for _, call := range accepted {
		current.toolIdentities[call.id] = toolIdentity{digest: call.digest, turn: turn}
	}
}

// Existing canonical calls are restored, not admitted as new proposals. The
// same preflight detects ambiguous/corrupt history before it can execute.
func (current *coordinator) restoreToolAdmission(item sessionstore.Item) error {
	if item.Kind == sessionstore.ItemModelResponse {
		response, ok := item.Data.(sessionstore.ModelResponse)
		if !ok {
			return fmt.Errorf("model response data is %T, want sessionstore.ModelResponse: %w", item.Data, llm.ErrToolRecoveryRequired)
		}
		accepted, err := current.checkToolAdmission(response.Response)
		if err != nil {
			return err
		}
		current.rememberToolAdmission(response.TurnID, accepted)
	}
	if item.Kind == sessionstore.ItemToolCallStatus {
		status, ok := item.Data.(sessionstore.ToolCallStatus)
		if !ok {
			return fmt.Errorf("tool-call status data is %T, want sessionstore.ToolCallStatus: %w", item.Data, llm.ErrToolRecoveryRequired)
		}
		prior, exists := current.toolIdentities[status.CallID]
		if !exists || prior.turn != status.TurnID {
			return llm.ErrToolRecoveryRequired
		}
	}
	return nil
}
