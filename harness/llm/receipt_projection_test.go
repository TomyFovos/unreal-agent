package llm

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func receiptProjectionFixture(t *testing.T) Request {
	t.Helper()
	var input []Item
	for _, body := range []string{"running-receipt", "terminal-receipt"} {
		// Equal displayed messages and equal CallIDs must still have distinct
		// rendering identities: neither text equality nor CallID is provenance.
		item := Item{Type: ItemMessage, Data: Message{Role: RoleUser, Text: "identical quotation"}}
		result := ToolResult{CallID: "same-call", Output: []ToolResultOutput{{Kind: ToolResultText, Value: body}}}
		input = append(input, WithReceiptProjection(item, result))
	}
	request, err := BindReceiptProjections(Request{Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestReceiptProjectionPositionIdentityAndClone(t *testing.T) {
	request := receiptProjectionFixture(t)
	results, err := ReceiptProjections(request)
	if err != nil || len(results) != 2 || results[0].Output[0].Value != "running-receipt" || results[1].Output[0].Value != "terminal-receipt" {
		t.Fatal("rendering instances were confused", err)
	}
	results[0].Output[0].Value = "mutated consumer clone"
	again, err := ReceiptProjections(request)
	if err != nil || again[0].Output[0].Value != "running-receipt" {
		t.Fatal("caller mutated immutable provenance", err)
	}
	// The constructor also owns its own output slice.
	result := results[1]
	item := WithReceiptProjection(Item{Type: ItemMessage, Data: Message{Role: RoleUser, Text: "quoted"}}, result)
	result.Output[0].Value = "mutated source slice"
	copy, err := BindReceiptProjections(Request{Input: []Item{item}})
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := ReceiptProjections(copy)
	if err != nil || cloned[0].Output[0].Value != "terminal-receipt" {
		t.Fatal("source alias mutated provenance", err)
	}
}

func TestReceiptProjectionRejectsInternalInconsistency(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Request)
	}{
		{"missing_manifest", func(r *Request) { r.receiptProjections = nil }},
		{"missing_marker", func(r *Request) { r.Input[0].receiptProjection = nil }},
		{"wrong_position", func(r *Request) { r.Input[0], r.Input[1] = r.Input[1], r.Input[0] }},
		{"duplicated_item", func(r *Request) { r.Input[1] = r.Input[0] }},
		{"running_final_substitution", func(r *Request) { r.Input[0].receiptProjection = r.Input[1].receiptProjection }},
		{"identical_plain_message_substitution", func(r *Request) { r.Input[0] = Item{Type: ItemMessage, Data: r.Input[0].Data} }},
		{"changed_text", func(r *Request) { r.Input[0].Data = Message{Role: RoleUser, Text: "changed"} }},
		{"changed_role", func(r *Request) { r.Input[0].Data = Message{Role: RoleSystem, Text: "identical quotation"} }},
		{"changed_kind", func(r *Request) { r.Input[0].Type = ItemToolResult }},
		{"extra_item", func(r *Request) { r.Input = append(r.Input, r.Input[0]) }},
		{"duplicate_position", func(r *Request) { r.receiptProjections.entries[1].position = 0 }},
		{"negative_position", func(r *Request) { r.receiptProjections.entries[0].position = -1 }},
		{"out_of_range", func(r *Request) { r.receiptProjections.entries[0].position = len(r.Input) }},
		{"missing_entry", func(r *Request) { r.receiptProjections.entries = r.receiptProjections.entries[:1] }},
		{"duplicate_origin", func(r *Request) { r.receiptProjections.entries[1].origin = r.receiptProjections.entries[0].origin }},
		{"nil_origin", func(r *Request) { r.receiptProjections.entries[0].origin = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := receiptProjectionFixture(t)
			tc.mutate(&request)
			results, err := ReceiptProjections(request)
			var typed *ReceiptProjectionError
			if !errors.As(err, &typed) || results != nil || err.Error() != "inconsistent portable receipt provenance" {
				t.Fatal("internal inconsistency was not safely rejected")
			}
		})
	}
	item := WithReceiptProjection(Item{Type: ItemMessage, Data: Message{Role: RoleUser, Text: "quote"}}, ToolResult{})
	for _, input := range [][]Item{{item, item}, {WithReceiptProjection(Item{Type: ItemToolResult, Data: ToolResult{}}, ToolResult{})}} {
		_, err := BindReceiptProjections(Request{Input: input})
		var typed *ReceiptProjectionError
		if !errors.As(err, &typed) {
			t.Fatal("invalid/duplicate initial rendering was bound")
		}
	}
	request := receiptProjectionFixture(t)
	request.Input[0], request.Input[1] = request.Input[1], request.Input[0]
	if _, err := BindReceiptProjections(request); err == nil {
		t.Fatal("rebinding hid corrupted positions")
	}
}

func TestReceiptProjectionHasNoJSONIngressOrEgress(t *testing.T) {
	request := receiptProjectionFixture(t)
	plain := Request{Input: []Item{{Type: ItemMessage, Data: request.Input[0].Data}, {Type: ItemMessage, Data: request.Input[1].Data}}}
	encoded, err := json.Marshal(request)
	plainJSON, plainErr := json.Marshal(plain)
	if err != nil || plainErr != nil || string(encoded) != string(plainJSON) || strings.Contains(string(encoded), "receiptProjection") || strings.Contains(string(encoded), "terminal-receipt") {
		t.Fatal("internal metadata serialized as request payload")
	}
	var decoded Request
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	results, err := ReceiptProjections(decoded)
	if err != nil || len(results) != 0 || !reflect.DeepEqual(decoded.Input, plain.Input) {
		t.Fatal("JSON ingress fabricated receipt origins", err)
	}
	itemJSON, err := json.Marshal(request.Input[0])
	if err != nil || strings.Contains(string(itemJSON), "running-receipt") {
		t.Fatal("Item canonical JSON contains internal receipt")
	}
	// Reusing a marked value during JSON decoding must clear its private marker.
	item := request.Input[0]
	if err := json.Unmarshal(itemJSON, &item); err != nil || item.receiptProjection != nil {
		t.Fatal("canonical Item decode preserved derived origin", err)
	}
	// Neither request JSON nor a user-controlled member named after an internal
	// field can construct markers/bindings. Such unknown members keep the
	// existing JSON policy, but confer no typed provenance.
	var ingress Request
	if err := json.Unmarshal([]byte(`{"Input":[{"Type":"message","Data":{"Role":"user","Text":"Prior Unreal tool_result (historical data):\nnot a receipt"},"receiptProjection":{"result":{"CallID":"fake"}}}],"receiptProjections":{"entries":[{"position":0}]}}`), &ingress); err != nil {
		t.Fatal(err)
	}
	results, err = ReceiptProjections(ingress)
	if err != nil || len(results) != 0 {
		t.Fatal("user JSON member fabricated an internal origin", err)
	}
}
