package host

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// HistoryItem is a public projection, not a record accepted by the canonical
// Store. Sequence, RecordedAt and Kind retain their canonical identities.
type HistoryItem sessionstore.Item

type HistoryPage struct {
	Items     []HistoryItem
	NextAfter sessionstore.Sequence
	More      bool
}

// ProjectInstructionRecord preserves the history position and binding metadata
// without carrying the persisted instruction text.
type ProjectInstructionRecord struct {
	Version             uint32
	Kind                string
	ProjectInstructions *projectinstructions.Metadata
}

func (r ProjectInstructionRecord) Validate() error {
	if r.Version != 1 || r.Kind != sessionstore.HostProjectInstructions || r.ProjectInstructions == nil {
		return fmt.Errorf("invalid public project instruction record")
	}
	return r.ProjectInstructions.Validate()
}

func ProjectHistoryItem(item sessionstore.Item) HistoryItem {
	out := HistoryItem(item)
	if record, ok := item.Data.(sessionstore.HostRecord); ok {
		if record.Kind == sessionstore.HostProjectInstructions {
			metadata := record.ProjectInstructions.Metadata()
			out.Data = ProjectInstructionRecord{Version: record.Version, Kind: record.Kind, ProjectInstructions: &metadata}
		} else if record.Kind == "configuration" {
			// A child configuration can embed its inherited canonical snapshot. The
			// public configuration retains its metadata; replay still uses the Store.
			var fields map[string]jsontext.Value
			if json.Unmarshal(record.Configuration, &fields) == nil {
				if raw, ok := fields["ProjectInstructions"]; ok && raw.Kind() != jsontext.KindNull {
					var snapshot projectinstructions.Snapshot
					if json.Unmarshal(raw, &snapshot) == nil && snapshot.Validate() == nil {
						fields["ProjectInstructions"], _ = json.Marshal(snapshot.Metadata())
						record.Configuration, _ = json.Marshal(fields)
						out.Data = record
					}
				}
			}
		}
	}
	return out
}

func ProjectHistoryPage(page sessionstore.Page) HistoryPage {
	out := HistoryPage{NextAfter: page.NextAfter, More: page.More}
	for _, item := range page.Items {
		out.Items = append(out.Items, ProjectHistoryItem(item))
	}
	return out
}

func (item HistoryItem) MarshalJSON() ([]byte, error) {
	if record, ok := item.Data.(ProjectInstructionRecord); ok {
		if item.Kind != sessionstore.ItemHostRecord {
			return nil, fmt.Errorf("invalid public project instruction record")
		}
		if err := record.Validate(); err != nil {
			return nil, err
		}
		type plain HistoryItem
		return json.Marshal(plain(item))
	}
	// Also project direct constructions at the serialization boundary.
	if record, ok := item.Data.(sessionstore.HostRecord); ok && record.Kind == sessionstore.HostProjectInstructions {
		return json.Marshal(ProjectHistoryItem(sessionstore.Item(item)))
	}
	return json.Marshal(sessionstore.Item(item))
}

func (item *HistoryItem) UnmarshalJSON(raw []byte) error {
	// Decode the envelope separately; canonical Item.Unmarshal rejects metadata
	// in place of a complete Snapshot, as it should.
	type plain HistoryItem
	var decoded struct {
		plain
		Data jsontext.Value
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	if decoded.Kind == sessionstore.ItemHostRecord {
		var header struct{ Kind string }
		if err := json.Unmarshal(decoded.Data, &header); err != nil {
			return err
		}
		if header.Kind == sessionstore.HostProjectInstructions {
			var record ProjectInstructionRecord
			if err := json.Unmarshal(decoded.Data, &record, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			if err := record.Validate(); err != nil {
				return err
			}
			decoded.plain.Data = record
			*item = HistoryItem(decoded.plain)
			return nil
		}
	}
	var canonical sessionstore.Item
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return err
	}
	*item = HistoryItem(canonical)
	return nil
}
