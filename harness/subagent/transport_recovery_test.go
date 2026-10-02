package subagent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

type transportIdleModel struct{}

func (transportIdleModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{}, nil
}

func TestTransportDisconnectAtSendAppendAndACK(t *testing.T) {
	for _, cut := range []string{"send_before", "send_after", "append_before", "append_after", "ack_before", "ack_after"} {
		t.Run(cut, func(t *testing.T) {
			dir := t.TempDir()
			open := func(mode host.Mode) (*host.Host, *host.Session) {
				h, err := host.New(t.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
					return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: transportIdleModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
				}})
				if err != nil {
					t.Fatal(err)
				}
				s, err := h.Open(t.Context(), host.Options{ID: "receiver", Mode: mode, Policy: permission.Unrestricted()})
				if err != nil {
					h.Close()
					t.Fatal(err)
				}
				return h, s
			}
			h, receiver := open(host.Create)
			fault := errors.New("injected channel loss")
			payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: "sender", Target: "receiver", Handle: "start", Kind: "message", Text: "canonical payload"})
			input := inbox.Input{ID: "stable", Kind: inbox.InputPeer, Payload: payload}
			var committed *host.Receipt
			connect := func(generation string, broken bool) *endpoint {
				sender := &endpoint{ctx: t.Context(), connection: generation, local: "sender", remote: "receiver"}
				target := &endpoint{ctx: t.Context(), connection: generation, local: "receiver", remote: "sender"}
				target.receive = func(ctx context.Context, in inbox.Input) (host.Receipt, error) {
					if broken && cut == "append_before" {
						return host.Receipt{}, fault
					}
					r, err := receiver.SubmitPeer(ctx, receiver.Generation, "sender", "start", in)
					if err == nil {
						committed = &r
					}
					if broken && cut == "append_after" {
						return host.Receipt{}, fault
					}
					return r, err
				}
				target.write = func(_ context.Context, f frame) error {
					if broken && cut == "ack_before" {
						return fault
					}
					if err := sender.accept(f); err != nil {
						return err
					}
					if broken && cut == "ack_after" {
						return fault
					}
					return nil
				}
				sender.write = func(_ context.Context, f frame) error {
					if broken && cut == "send_before" {
						return fault
					}
					// Serialize and decode the actual bounded wire frame before the
					// receiver commit; no receipt is manufactured by the fixture.
					wire, err := encodeFrame(f)
					if err != nil {
						return err
					}
					var decoder frameDecoder
					frames, err := decoder.push(wire)
					if err != nil || len(frames) != 1 {
						t.Fatal(err)
					}
					if broken && cut == "send_after" {
						return fault
					}
					return target.accept(frames[0])
				}
				return sender
			}
			if _, err := connect("old", true).send(t.Context(), input); err == nil {
				t.Fatal("uncertain delivery acknowledged")
			}
			h.Close()
			h, receiver = open(host.Resume)
			defer h.Close()
			// A new endpoint loses its volatile sent-ID cache. Receiver canonical
			// dedup, rather than a sender mutex, must reject changed payloads.
			if committed != nil {
				changed := input
				changed.Payload, _ = json.Marshal(inbox.PeerMessage{Version: 1, Sender: "sender", Target: "receiver", Handle: "start", Kind: "message", Text: "changed"})
				if _, err := connect("changed", false).send(t.Context(), changed); !errors.Is(err, host.ErrConflict) {
					t.Fatal("receiver conflict not typed", err)
				}
			}
			prior := committed
			ep := connect("new", false)
			first, err := ep.send(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			again, err := ep.send(t.Context(), input)
			if err != nil || first != again || prior != nil && first != *prior {
				t.Fatal("stable receipt lost", first, again, prior, err)
			}
			v, err := receiver.Inspect(0, 256)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, item := range v.History.Items {
				if in, ok := item.Data.(inbox.Input); ok && in.ID == input.ID {
					count++
					if in.Kind != inbox.InputPeer || !sameJSON(in.Payload, input.Payload) {
						t.Fatal("provenance or payload changed")
					}
				}
			}
			if count != 1 {
				t.Fatal("canonical input loss/duplicate", count)
			}
		})
	}
}
