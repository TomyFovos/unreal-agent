package llm

import "errors"

// RecoveryProvider is an opt-in model projection capability, never execution
// authority. Existing providers without the marker retain their original path.
func RecoveryProvider(adapter Adapter) string {
	if capable, ok := adapter.(interface{ ConversationRecoveryProvider() string }); ok {
		return capable.ConversationRecoveryProvider()
	}
	return ""
}

func (e *ContinuationError) ConversationRecoveryRequired() bool { return true }
func NeedsConversationRecovery(err error) bool {
	var marked interface {
		error
		ConversationRecoveryRequired() bool
	}
	return errors.As(err, &marked) && marked.ConversationRecoveryRequired()
}
