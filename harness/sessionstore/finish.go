package sessionstore

import (
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"strings"
)

// FinishResult is the canonical child report. It is independent of
// model-visible output truncation and contains no credential/configuration data.
type FinishResult struct {
	Status       string
	Summary      string
	ChangedFiles []string
	Tests        []string
	Blockers     []string
}
type FinishRecord struct {
	Version     uint32
	OperationID operation.ID
	Result      FinishResult
	ModelTurnID session.TurnID `json:",omitzero"`
}

func (r FinishRecord) Validate() error {
	if r.Version != 1 || r.OperationID == "" && r.ModelTurnID == "" || r.OperationID != "" && r.ModelTurnID != "" {
		return fmt.Errorf("invalid finish identity")
	}
	return r.Result.Validate()
}
func (r FinishResult) Validate() error {
	if (r.Status != "completed" && r.Status != "failed") || strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("invalid finish result")
	}
	return nil
}
