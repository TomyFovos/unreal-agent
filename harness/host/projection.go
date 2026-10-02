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
			record.Configuration = projectConfiguration(record.Configuration)
			out.Data = record
		}
	}
	return out
}

// projectConfiguration never returns an untrusted instruction value. Null keeps
// the field's presence without claiming that an unknown snapshot has a valid
// identity. Already-projected metadata is accepted strictly for idempotency.
func projectConfiguration(raw jsontext.Value) jsontext.Value {
	null := jsontext.Value("null")
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		// An unreadable object cannot be inspected safely; retain the history
		// envelope and redact only its public configuration.
		return null
	}
	instructions, ok := fields["ProjectInstructions"]
	if !ok {
		return raw
	}
	fields["ProjectInstructions"] = null
	var snapshot projectinstructions.Snapshot
	var metadata projectinstructions.Metadata
	valid := false
	if json.Unmarshal(instructions, &snapshot) == nil && snapshot.Validate() == nil {
		metadata, valid = snapshot.Metadata(), true
	} else if json.Unmarshal(instructions, &metadata, json.RejectUnknownMembers(true)) == nil && metadata.Validate() == nil {
		// Content (including malformed Content) is an unknown metadata member.
		// It must not bypass full Snapshot validation via this fallback.
		valid = true
	}
	if valid {
		if data, err := json.Marshal(metadata); err == nil {
			fields["ProjectInstructions"] = data
		}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return null
	}
	return data
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
	if record, ok := item.Data.(sessionstore.HostRecord); ok {
		switch record.Kind {
		case sessionstore.HostProjectInstructions:
			return json.Marshal(ProjectHistoryItem(sessionstore.Item(item)))
		case "configuration":
			return json.Marshal(sessionstore.Item(ProjectHistoryItem(sessionstore.Item(item))))
		}
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
