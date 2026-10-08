package sessionstore

import (
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"strings"
)

// FinishResult is the canonical, bounded child report. It is independent of
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
	if (r.Status != "completed" && r.Status != "failed") || strings.TrimSpace(r.Summary) == "" || len(r.Summary) > 32768 {
		return fmt.Errorf("invalid finish result")
	}
	total := len(r.Summary)
	for _, list := range [][]string{r.ChangedFiles, r.Tests, r.Blockers} {
		if len(list) > 256 {
			return fmt.Errorf("too many finish items")
		}
		for _, v := range list {
			if len(v) > 4096 {
				return fmt.Errorf("finish item too large")
			}
			total += len(v)
		}
	}
	if total > 131072 {
		return fmt.Errorf("finish result too large")
	}
	return nil
}
