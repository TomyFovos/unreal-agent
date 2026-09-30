package subagent

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"os"
)

type ChildView struct {
	View          host.View
	Configuration *ChildConfig
	Finish        *sessionstore.FinishRecord
}

// ReadChild never acquires execution ownership. An empty generation means
// runtime ownership is unknown, not that the child is currently idle.
func ReadChild(ctx context.Context, directory string, id session.ID, after sessionstore.Sequence, limit int) (ChildView, error) {
	if limit < 1 || limit > 256 {
		return ChildView{}, fmt.Errorf("child page limit must be 1..256")
	}
	if _, err := os.Stat(directory); err != nil {
		return ChildView{}, err
	}
	store, err := localfile.New(directory)
	if err != nil {
		return ChildView{}, err
	}
	snapshot, err := store.Inspect(ctx, id)
	if err != nil {
		return ChildView{}, err
	}
	page, err := store.Items(ctx, id, after, limit)
	if err != nil {
		return ChildView{}, err
	}
	ops, err := store.Operations(ctx, id)
	if err != nil {
		return ChildView{}, err
	}
	result := ChildView{View: host.View{Session: snapshot, History: page, Operations: ops}}
	for cursor := sessionstore.BeforeFirst; ; {
		p, err := store.Items(ctx, id, cursor, 256)
		if err != nil {
			return ChildView{}, err
		}
		for _, item := range p.Items {
			if item.Kind == sessionstore.ItemFork {
				result.Configuration = nil
				result.Finish = nil
			}
			if item.Kind != sessionstore.ItemHostRecord {
				continue
			}
			record := item.Data.(sessionstore.HostRecord)
			switch record.Kind {
			case "configuration":
				var config ChildConfig
				if json.Unmarshal(record.Configuration, &config) == nil && config.Validate() == nil && config.ChildID == id {
					result.Configuration = &config
				}
			case sessionstore.HostProjectInstructions:
				metadata := record.ProjectInstructions.Metadata()
				result.View.ProjectInstructions = &metadata
			case "finish":
				result.Finish = record.Finish
			}
		}
		if !p.More {
			break
		}
		cursor = p.NextAfter
	}
	return result, nil
}
