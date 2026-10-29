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
	result, err := ReadChildSnapshot(ctx, directory, id)
	if err != nil {
		return ChildView{}, err
	}
	items := result.View.History.Items
	start := min(uint64(after), uint64(len(items)))
	end := min(start+uint64(limit), uint64(len(items)))
	next := after
	if end > start {
		next = items[end-1].Sequence
	}
	result.View.History = host.HistoryPage{Items: items[start:end], NextAfter: next, More: end < uint64(len(items))}
	return result, nil
}

// ReadChildSnapshot reads a complete committed observation once. A scoped
// viewer may reuse it while the log's identity, size and modification time are
// unchanged. It never acquires writer ownership or infers runtime liveness.
func ReadChildSnapshot(ctx context.Context, directory string, id session.ID) (ChildView, error) {
	if _, err := os.Stat(directory); err != nil {
		return ChildView{}, err
	}
	store, err := localfile.New(directory)
	if err != nil {
		return ChildView{}, err
	}
	snapshot, items, ops, err := store.ReadSnapshot(ctx, id)
	if err != nil {
		return ChildView{}, err
	}
	page := sessionstore.Page{Items: items, NextAfter: sessionstore.Sequence(len(items))}
	result := ChildView{View: host.View{Session: snapshot, History: host.ProjectHistoryPage(page), Operations: ops}}
	for _, item := range items {
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
	return result, nil
}
