package llm

// Progress belongs to one Respond invocation. Reset starts a transport attempt;
// consumers must discard the prior attempt's partial text. Unsupported adapters
// may ignore the callback and return only their completed Response.
type Progress struct {
	Attempt uint64
	Reset   bool
	Delta   string
}
