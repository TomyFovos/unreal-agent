package llm

import "context"

type RequestOptions struct {
	CacheKey string
	// Progress is optional, ephemeral and never canonical history. Callbacks must be fast.
	Progress func(Progress)
}

type Adapter interface {
	Respond(context.Context, Request, RequestOptions) (Response, error)
}
