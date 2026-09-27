package profile

import (
	"context"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// Measurement records actual adapter-reported usage, not a character/token
// estimate. Compare is opt-in; no profile is selected from benchmark results.
type Measurement struct {
	Profile  Selection
	Fixture  string
	Usage    llm.Usage
	Response llm.Response
}
type Fixture struct {
	Name    string
	Request llm.Request
}

func Compare(ctx context.Context, adapter llm.Adapter, profiles []Resolved, fixtures []Fixture) ([]Measurement, error) {
	out := []Measurement{}
	for _, p := range profiles {
		for _, f := range fixtures {
			req := f.Request
			req.Input = slices.Clone(req.Input)
			addition, tools := p.Compose("", req.Tools)
			req.Tools = tools
			req.Input = append([]llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: addition}}}, req.Input...)
			response, err := adapter.Respond(ctx, req, llm.RequestOptions{})
			if err != nil {
				return nil, err
			}
			out = append(out, Measurement{Profile: p.Selection(), Fixture: f.Name, Usage: response.Usage, Response: response})
		}
	}
	return out, nil
}
