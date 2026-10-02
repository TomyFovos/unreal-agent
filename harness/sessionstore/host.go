package sessionstore

import (
	"encoding/json/jsontext"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
)

// HostRecord is canonical configuration or completion of a persisted stop.
// It carries no credential values or runtime handles.
type HostRecord struct {
	Version       uint32
	Kind          string
	Configuration jsontext.Value `json:",omitzero"`
	Inputs        []inbox.ID     `json:",omitzero"`
}

const ItemHostRecord ItemKind = "host_record"

func (r HostRecord) Validate() error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported host record version %d", r.Version)
	}
	switch r.Kind {
	case "configuration":
		if len(r.Configuration) == 0 || !r.Configuration.IsValid() || len(r.Inputs) != 0 {
			return fmt.Errorf("invalid host configuration record")
		}
	case "stop_complete":
		if len(r.Inputs) == 0 || len(r.Configuration) != 0 {
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
