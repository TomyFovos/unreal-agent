package contextbuilder

import (
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/contextengine"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// Engine assigns a new ID to every immutable public Unit (including running
// and final receipts). Include the metadata that Engine can vary on candidate
// copies as well; neither a CallID nor a body/JSON hash is a Unit identity.
type projectionUnitKey struct {
	id, origin, group, parent, fingerprint string
	kind                                   contextengine.Kind
	class                                  contextengine.Class
	source                                 contextengine.HistoryRef
	tokens                                 int64
	required, staged, retrievalOnly        bool
}

type projectionPartnerKey struct {
	present bool
	call    llm.ToolCall
}

type projectionCostKey struct {
	unit              projectionUnitKey
	partner           projectionPartnerKey
	reason            string
	reference, quoted bool
}

func projectionKey(u contextengine.Unit, reason string, reference, quoted bool) projectionCostKey {
	return projectionCostKey{unit: projectionUnitKey{
		id: u.ID, origin: u.Origin, group: u.Group, parent: u.ParentID, fingerprint: u.Fingerprint,
		kind: u.Kind, class: u.Class, source: u.Source, tokens: u.Tokens,
		required: u.Required, staged: u.Staged, retrievalOnly: u.RetrievalOnly,
	}, reason: reason, reference: reference, quoted: quoted}
}

// One buildProjected invocation owns this memo. Its model, schemas, receipt
// identity, native replay and public partners are fixed for that invocation.
// Only successful marginal sizes are retained. Full selected/final projection
// and receipt provenance validation never use this memo.
type projectionMemo struct {
	measure   func([]llm.Item) (int64, error)
	costs     map[projectionCostKey]int64
	perUnit   map[string]int
	bases     map[projectionPartnerKey]int64
	baseLimit int
}

func newProjectionMemo(measure func([]llm.Item) (int64, error), partners int) *projectionMemo {
	return &projectionMemo{measure: measure, costs: map[projectionCostKey]int64{}, perUnit: map[string]int{}, bases: map[projectionPartnerKey]int64{}, baseLimit: partners + 1}
}

func (m *projectionMemo) cost(key projectionCostKey, render func() []llm.Item) (int64, error) {
	if value, ok := m.costs[key]; ok {
		return value, nil
	}
	var partner []llm.Item
	if key.partner.present {
		partner = []llm.Item{{Type: llm.ItemToolCall, Data: key.partner.call}}
	}
	base, cached := m.bases[key.partner]
	if !cached {
		var err error
		base, err = m.measure(partner)
		if err != nil {
			return 0, err
		}
		if len(m.bases) < m.baseLimit {
			m.bases[key.partner] = base
		}
	}
	value, err := m.measure(append(slices.Clone(partner), render()...))
	if err != nil {
		return 0, err
	}
	if value < base {
		return 0, &contextengine.Error{Code: "invalid_projection_cost"}
	}
	value = value - base + 2 // unchanged array commas/join framing
	// Bounded by candidate Units, not arbitrary reason strings or history bodies.
	// On overflow, recompute instead of changing the projection/error contract.
	if key.unit.id != "" && m.perUnit[key.unit.id] < 16 {
		m.costs[key] = value
		m.perUnit[key.unit.id]++
	}
	return value, nil
}
