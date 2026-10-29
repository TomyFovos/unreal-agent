package llm

// ContinuationError contains a fixed structural reason, never provider IDs,
// call arguments, response bodies or private reasoning.
type ContinuationError struct{ Code string }

func (e *ContinuationError) Error() string { return "model input: " + e.Code }
