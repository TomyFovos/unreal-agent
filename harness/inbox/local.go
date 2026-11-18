package inbox

import (
	"context"
	"fmt"
)

type Inbox struct {
	ctx         context.Context
	submissions chan Input
	output      chan Input
	drains      chan chan []Input
}

var _ Writer = (*Inbox)(nil)

func New(ctx context.Context, seenIDs []ID) (*Inbox, error) {
	seen := make(map[ID]struct{}, len(seenIDs))
	for _, id := range seenIDs {
		if id == "" {
			return nil, fmt.Errorf("seen input ID is empty")
		}
		seen[id] = struct{}{}
	}
	inputs := &Inbox{
		ctx:         ctx,
		submissions: make(chan Input),
		output:      make(chan Input),
		drains:      make(chan chan []Input),
	}
	go inputs.run(seen, inputs.submissions)
	return inputs, nil
}

func (inputs *Inbox) Submit(ctx context.Context, input Input) error {
	if err := input.Validate(); err != nil {
		return fmt.Errorf("submit input: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := context.Cause(inputs.ctx); err != nil {
		return err
	}
	input = cloneInput(input)

	select {
	case inputs.submissions <- input:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-inputs.ctx.Done():
		return context.Cause(inputs.ctx)
	}
}

func (inputs *Inbox) Output() <-chan Input {
	return inputs.output
}

// Drain gives the sole consumer a FIFO snapshot before a model request. It
// includes submissions already accepted by the Inbox, even if Output had not
// yet been read. Output also tells the consumer when the Inbox closes.
func (inputs *Inbox) Drain(ctx context.Context) ([]Input, error) {
	reply := make(chan []Input, 1)
	select {
	case inputs.drains <- reply:
		return <-reply, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-inputs.ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (inputs *Inbox) run(seen map[ID]struct{}, submissions <-chan Input) {
	defer close(inputs.output)

	queued := make([]Input, 0)
	for {
		var output chan Input
		var next Input
		if len(queued) != 0 {
			output = inputs.output
			next = queued[0]
		}

		select {
		case input := <-submissions:
			if _, duplicate := seen[input.ID]; duplicate {
				continue
			}
			seen[input.ID] = struct{}{}
			queued = append(queued, input)

		case output <- next:
			queued[0] = Input{}
			queued = queued[1:]

		case reply := <-inputs.drains:
			reply <- queued
			queued = nil

		case <-inputs.ctx.Done():
			return
		}
	}
}

func cloneInput(input Input) Input {
	input.Payload = input.Payload.Clone()
	return input
}
