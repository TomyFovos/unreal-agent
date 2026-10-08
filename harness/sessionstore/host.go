package sessionstore

import (
	"encoding/json/jsontext"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
)

// HostRecord is canonical configuration, completion of a persisted stop, or
// the project instructions a session was created with. It carries no
// credential values or runtime handles.
type HostRecord struct {
	Version             uint32
	Kind                string
	Configuration       jsontext.Value                `json:",omitzero"`
	Finish              *FinishRecord                 `json:",omitzero"`
	Inputs              []inbox.ID                    `json:",omitzero"`
	ProjectInstructions *projectinstructions.Snapshot `json:",omitzero"`
	Selection           *RuntimeSelection             `json:",omitzero"`
}

const HostProjectInstructions = "project_instructions"

const ItemHostRecord ItemKind = "host_record"

func (r HostRecord) Validate() error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported host record version %d", r.Version)
	}
	if r.Kind != HostProjectInstructions && r.ProjectInstructions != nil {
		return fmt.Errorf("host record %q carries project instructions", r.Kind)
	}
	if r.Kind != HostRuntimeSelection && r.Kind != HostRuntimeApplied && r.Selection != nil {
		return fmt.Errorf("unexpected runtime selection")
	}
	switch r.Kind {
	case HostRuntimeSelection, HostRuntimeApplied:
		if r.Selection == nil || r.Selection.Revision == 0 || r.Selection.RequestID == "" || len(r.Configuration) != 0 || r.Finish != nil || len(r.Inputs) != 0 || r.ProjectInstructions != nil {
			return fmt.Errorf("invalid runtime selection record")
		}
		return r.Selection.Validate()
	case HostProjectInstructions:
		if r.ProjectInstructions == nil || len(r.Configuration) != 0 || len(r.Inputs) != 0 || r.Finish != nil {
			return fmt.Errorf("invalid project instructions record")
		}
		if err := r.ProjectInstructions.Validate(); err != nil {
			return err
		}
	case "finish":
		if r.Finish == nil || len(r.Configuration) != 0 || len(r.Inputs) != 0 {
			return fmt.Errorf("invalid finish record")
		}
		return r.Finish.Validate()
	case "configuration":
		if len(r.Configuration) == 0 || !r.Configuration.IsValid() || len(r.Inputs) != 0 || r.Finish != nil {
			return fmt.Errorf("invalid host configuration record")
		}
	case "stop_complete":
		if len(r.Inputs) == 0 || len(r.Configuration) != 0 || r.Finish != nil {
			return fmt.Errorf("invalid stop completion")
		}
		for _, id := range r.Inputs {
			if id == "" {
				return fmt.Errorf("empty stop input ID")
			}
		}
	default:
		return fmt.Errorf("unsupported host record kind %q", r.Kind)
	}
	return nil
}
