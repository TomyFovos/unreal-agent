package coordinator

import (
	"context"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// The conversation-epoch fixtures replay Sessions recorded by the removed
// Claude Native route.
type conversationAdapter struct{ llm.Adapter }

func (conversationAdapter) RequiresFullToolBatch() bool          { return true }
func (conversationAdapter) ConversationRecoveryProvider() string { return "claude-native" }

type conversationStore struct {
	*fakeStore
	failBefore, failAfter error
	epochs                int
}

func (s *conversationStore) AppendHostRecord(_ context.Context, _ session.ID, record sessionstore.HostRecord) error {
	if s.failBefore != nil {
		return s.failBefore
	}
	if err := record.Validate(); err != nil {
		return err
	}
	s.items = append(s.items, storedItem(sessionstore.Sequence(len(s.items)+1), sessionstore.ItemHostRecord, record))
	s.epochs++
	return s.failAfter
}
func newConversationFixture(t *testing.T) (*toolAdmissionFixture, *conversationStore) {
	t.Helper()
	f := newToolAdmissionFixture(t)
	s := &conversationStore{fakeStore: f.store}
	f.store.onAppendInput = func(input inbox.Input) {
		f.store.items = append(f.store.items, storedItem(sessionstore.Sequence(len(f.store.items)+1), sessionstore.ItemInput, input))
	}
	f.current.dependencies.Sessions = s
	f.current.dependencies.LLM = conversationAdapter{f.current.dependencies.LLM}
	f.current.dependencies.ContextBuilder.SetModel(llm.Model{ID: "synthetic-model"})
	if b, ok := f.current.dependencies.ContextBuilder.(interface{ SetRuntimeProvider(string) }); ok {
		b.SetRuntimeProvider("claude-native")
	}
	if err := f.current.handleInboxInput(t.Context(), externalEvent(t, 0, "goal", "continue synthetic development")); err != nil {
		t.Fatal(err)
	}
	return f, s
}

// continuityFailure stands in for the removed Native client's continuity
// error: it asks for a new conversation epoch.
type continuityFailure struct{}

func (continuityFailure) Error() string                      { return "synthetic continuity failure" }
func (continuityFailure) ConversationRecoveryRequired() bool { return true }
