package llm

import "time"

// ActivityRecord contains timing and runtime metadata, never commands, file
// contents, provider credentials, or tool arguments. Missing fields in older
// histories remain unknown.
type ActivityRecord struct {
	Kind      string
	ID        string    `json:",omitzero"`
	StartedAt time.Time `json:",omitzero"`
	EndedAt   time.Time `json:",omitzero"`
	Revision  string    `json:",omitzero"`
	Modified  bool      `json:",omitzero"`
	Provider  string    `json:",omitzero"`
	Model     string    `json:",omitzero"`
	Effort    string    `json:",omitzero"`
	Template  string    `json:",omitzero"`
	Tool      string    `json:",omitzero"`
	Reason    string    `json:",omitzero"`
	Usage     *Usage    `json:",omitzero"`
}
