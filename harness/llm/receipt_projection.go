package llm

import "slices"

// ReceiptProjectionError reports an internal derived-request inconsistency.
// It deliberately contains no item positions, IDs, text or receipt values.
type ReceiptProjectionError struct{}

func (*ReceiptProjectionError) Error() string { return "inconsistent portable receipt provenance" }

// Both markers and bindings are private and immutable after construction. A
// marker identifies an actual rendering instance, not its text or CallID. Two
// equal quotations, or running/final updates of one call, have distinct markers.
type receiptProjection struct {
	providerID string
	message    Message
	valid      bool
	result     ToolResult
}

type receiptProjectionBinding struct {
	position int
	origin   *receiptProjection
}

type receiptProjectionBindings struct {
	inputLength int
	entries     []receiptProjectionBinding
}

// WithReceiptProjection is for the trusted context renderer: bind a portable
// message to the adopted typed receipt that it actually rendered. It does not
// parse text, assign execution authority, or add a public/canonical field. The
// caller must apply source eligibility before rendering; the consuming adapter
// also checks the typed receipt independently. Binding the final request is a
// separate step, so dropped/reordered markers cannot silently become prose.
func WithReceiptProjection(item Item, result ToolResult) Item {
	message, ok := item.Data.(Message)
	result.Output = slices.Clone(result.Output)
	item.receiptProjection = &receiptProjection{providerID: item.ProviderID, message: message, valid: ok && item.Type == ItemMessage, result: result}
	return item
}

// BindReceiptProjections seals the final selected/rendered input positions.
// Builders call this once per derived request, including legacy projections.
// Existing bindings are validated rather than refreshed to hide corruption.
func BindReceiptProjections(request Request) (Request, error) {
	if request.receiptProjections != nil {
		_, err := ReceiptProjections(request)
		return request, err
	}
	var entries []receiptProjectionBinding
	seen := map[*receiptProjection]bool{}
	for position, item := range request.Input {
		origin := item.receiptProjection
		if origin == nil {
			continue
		}
		if seen[origin] || !matchesReceiptProjection(item, origin) {
			return Request{}, &ReceiptProjectionError{}
		}
		seen[origin] = true
		entries = append(entries, receiptProjectionBinding{position: position, origin: origin})
	}
	if len(entries) > 0 {
		request.receiptProjections = &receiptProjectionBindings{inputLength: len(request.Input), entries: entries}
	}
	return request, nil
}

// ReceiptProjections returns clones of the adopted safe receipt values at their
// exact request positions. A marker without a manifest, a missing marker, a
// duplicate origin, a moved item, or changed message is an internal error. Plain
// messages are never inspected for receipt-like headings, JSON or identifiers.
func ReceiptProjections(request Request) (map[int]ToolResult, error) {
	expected := map[int]*receiptProjection{}
	if bindings := request.receiptProjections; bindings != nil {
		if bindings.inputLength != len(request.Input) || len(bindings.entries) == 0 {
			return nil, &ReceiptProjectionError{}
		}
		seen := map[*receiptProjection]bool{}
		for _, entry := range bindings.entries {
			if entry.position < 0 || entry.position >= len(request.Input) || entry.origin == nil || expected[entry.position] != nil || seen[entry.origin] {
				return nil, &ReceiptProjectionError{}
			}
			expected[entry.position] = entry.origin
			seen[entry.origin] = true
		}
	}
	for position, item := range request.Input {
		origin := item.receiptProjection
		if origin != expected[position] || origin != nil && !matchesReceiptProjection(item, origin) {
			return nil, &ReceiptProjectionError{}
		}
	}
	results := make(map[int]ToolResult, len(expected))
	for position, origin := range expected {
		result := origin.result
		result.Output = slices.Clone(result.Output)
		results[position] = result
	}
	return results, nil
}

func matchesReceiptProjection(item Item, origin *receiptProjection) bool {
	message, ok := item.Data.(Message)
	return origin.valid && item.Type == ItemMessage && item.ProviderID == origin.providerID && ok && message == origin.message
}
