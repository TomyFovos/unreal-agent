// Package host owns long-lived sessions independently of client connections.
package host

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/coordinator"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/projectinstructions"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

var (
	ErrConflict        = errors.New("input ID payload conflict")
	ErrStaleGeneration = errors.New("stale session generation")
	ErrStopped         = errors.New("session stopped")
)

type Runtime struct {
	Builder    contextbuilder.Builder
	LLM        llm.Adapter
	Tools      tool.Registry
	Operations operation.Manager
	Close      func() error
}
type Factory func(context.Context, session.ID) (Runtime, error)
type Config struct {
	Directory string
	Build     Factory
}
type Mode string

const (
	Create         Mode = "create"
	Resume         Mode = "resume"
	CreateOrResume Mode = "open"
)

type Options struct {
	// Policy is an explicit immutable execution capability set. Nil denies all.
	// Its owner closes it after Host execution has stopped.
	Policy *permission.Policy

	Lifecycle string
	ID        session.ID
	Mode      Mode
	// Configuration contains non-secret, resolved runtime identity only.
	Configuration jsontext.Value
	Initial       []inbox.Input
	Heartbeat     time.Duration

	// Workspace, when set, is the root whose AGENTS.md a newly created session
	// binds. Resume, restart, and fork replay the persisted snapshot instead.
	Workspace string
	// ProjectInstructions binds this snapshot to a newly created session
	// instead of discovering one, so a child shares its parent's revision.
	ProjectInstructions *projectinstructions.Snapshot
}

// creationRecords are bound atomically with a new session's header. Discovery
// failures fail creation; they are never skipped or truncated.
func (o Options) creationRecords() ([]sessionstore.HostRecord, error) {
	var snapshot projectinstructions.Snapshot
	switch {
	case o.ProjectInstructions != nil:
		snapshot = *o.ProjectInstructions
	case o.Workspace != "":
		var err error
		if snapshot, err = projectinstructions.Discover(o.Workspace); err != nil {
			return nil, err
		}
	default:
		return nil, nil
	}
	return []sessionstore.HostRecord{{Version: 1, Kind: sessionstore.HostProjectInstructions, ProjectInstructions: &snapshot}}, nil
}
type Host struct {
	ctx      context.Context
	cancel   context.CancelFunc
	config   Config
	mu       sync.Mutex
	sessions map[session.ID]*Session
	closed   bool
}
type Receipt struct {
	ID       inbox.ID
	Sequence sessionstore.Sequence
}
type submission struct {
	input     inbox.Input
	receipt   Receipt
	done      chan struct{}
	err       error
	committed bool
}
type Event struct {
	Progress   *Progress `json:",omitzero"`
	Generation string
	Revision   uint64
	Kind       string
	Item       *sessionstore.Item   `json:",omitzero"`
	Operation  *operation.Operation `json:",omitzero"`
}
type View struct {
	Progress   *Progress `json:",omitzero"`
	Session    sessionstore.Snapshot
	Generation string
	Revision   uint64
	History    sessionstore.Page
	Operations []operation.Operation
	Running    bool
	Failure    string `json:",omitzero"`

	// ProjectInstructions identifies the bound snapshot without its content.
	ProjectInstructions *projectinstructions.Metadata `json:",omitzero"`
}
type Subscription struct {
	Initial View
	Events  <-chan Event
	Cancel  func()
}
type Session struct {
	ID             session.ID
	Generation     string
	StartAfter     sessionstore.Sequence
	ctx            context.Context
	cancel         context.CancelFunc
	lock           *os.File
	store          *localfile.Store
	inbox          *inbox.Inbox
	runtime        Runtime
	mu             sync.Mutex
	snapshot       sessionstore.Snapshot
	items          []sessionstore.Item
	operations     map[operation.ID]operation.Operation
	submissions    map[inbox.ID]*submission
	subscribers    map[uint64]chan Event
	nextSubscriber uint64
	progress       *Progress
	instructions   *projectinstructions.Metadata
	progressEpoch  uint64
	revision       uint64
	done           chan struct{}
	running        bool
	err            error
}

func New(ctx context.Context, c Config) (*Host, error) {
	if c.Build == nil {
		return nil, fmt.Errorf("host runtime factory is required")
	}
	if _, err := localfile.New(c.Directory); err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	return &Host{ctx: child, cancel: cancel, config: c, sessions: map[session.ID]*Session{}}, nil
}
func (h *Host) Open(ctx context.Context, o Options) (result *Session, err error) {
	if o.ID == "" {
		o.ID = session.ID(uuid.New().String())
	}
	if o.Mode == "" {
		o.Mode = CreateOrResume
	}
	if o.Mode != Create && o.Mode != Resume && o.Mode != CreateOrResume {
		return nil, fmt.Errorf("unsupported open mode")
	}
	if o.Heartbeat < 0 {
		return nil, fmt.Errorf("negative heartbeat")
	}
	if err = context.Cause(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrStopped
	}
	if old := h.sessions[o.ID]; old != nil {
		select {
		case <-old.done:
		default:
			return nil, localfile.ErrWriterOwned
		}
	}
	raw, err := localfile.New(h.config.Directory)
	if err != nil {
		return nil, err
	}
	lock, err := raw.AcquireWriter(o.ID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			lock.Close()
		}
	}()
	restored, err := raw.Resume(ctx, o.ID)
	created := false
	if err == nil && o.Mode == Create {
		return nil, fs.ErrExist
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) || o.Mode == Resume {
			return nil, err
		}
		records, e := o.creationRecords()
		if e != nil {
			return nil, e
		}
		snapshot, e := raw.CreateWithHostRecords(ctx, o.ID, records...)
		if e != nil {
			return nil, e
		}
		restored = sessionstore.ResumeState{Snapshot: snapshot}
		created = true
	} else {
		// Explicit non-destructive v2 -> v3 migration; the original remains .v2.
		if err = raw.Upgrade(ctx, o.ID); err != nil {
			return nil, err
		}
	}
	cctx, cancel := context.WithCancel(permission.WithPolicy(h.ctx, o.Policy))
	s := &Session{ID: o.ID, Generation: uuid.New().String(), ctx: cctx, cancel: cancel, lock: lock, store: raw,
		snapshot: restored.Snapshot, operations: map[operation.ID]operation.Operation{}, submissions: map[inbox.ID]*submission{},
		subscribers: map[uint64]chan Event{}, done: make(chan struct{}), running: true}
	defer func() {
		if err != nil {
			cancel()
			if s.runtime.Close != nil {
				s.runtime.Close()
			}
			if manager, ok := s.runtime.Operations.(interface{ Done() <-chan struct{} }); ok {
				<-manager.Done()
			}
		}
	}()
	for after := sessionstore.BeforeFirst; ; {
		page, e := raw.Items(ctx, o.ID, after, 256)
		if e != nil {
			return nil, e
		}
		for _, item := range page.Items {
			s.rememberItem(item, false)
		}
		if !page.More {
			break
		}
		after = page.NextAfter
	}
	ops, e := raw.Operations(ctx, o.ID)
	if e != nil {
		return nil, e
	}
	for _, op := range ops {
		s.operations[op.ID] = op
	}
	raw.AddObserver(func(_ session.ID, item sessionstore.Item) { s.rememberItem(item, true) })
	// A new session's creation bindings are part of this run's output.
	if !created {
		s.StartAfter = sessionstore.Sequence(len(s.items))
	}
	if err = s.configure(ctx, o.Configuration); err != nil {
		return nil, err
	}
	seen := make([]inbox.ID, 0, len(s.submissions))
	for id := range s.submissions {
		seen = append(seen, id)
	}
	s.inbox, err = inbox.New(cctx, seen)
	if err != nil {
		return nil, err
	}
	s.runtime, err = h.config.Build(cctx, o.ID)
	if err != nil {
		return nil, err
	}
	if s.runtime.Builder == nil || s.runtime.LLM == nil || s.runtime.Tools == nil || s.runtime.Operations == nil {
		return nil, fmt.Errorf("incomplete session runtime")
	}
	s.runtime.LLM = s.withProgress(s.runtime.LLM)
	if o.Lifecycle == "" {
		o.Lifecycle = "interactive"
	}
	if builder, ok := s.runtime.Builder.(interface{ SetLifecycle(string) error }); ok {
		if err = builder.SetLifecycle(o.Lifecycle); err != nil {
			return nil, err
		}
	}
	// Register the initial batch without asynchronously forwarding individual
	// messages. Coordinator consumes all of it before its first model decision.
	var initial []inbox.Input
	for _, input := range o.Initial {
		pending, fresh, e := s.register(input)
		if e != nil {
			return nil, e
		}
		if fresh {
			initial = append(initial, pending.input)
		}
	}
	pending := s.pendingStop()
	dependencies := coordinator.Dependencies{SessionID: o.ID, Inbox: s.inbox, Restored: restored,
		Sessions: &serializedStore{s}, ContextBuilder: s.runtime.Builder, LLM: s.runtime.LLM, Tools: s.runtime.Tools,
		Operations: s.runtime.Operations, ToolHeartbeatInterval: o.Heartbeat, RestoredStop: pending, InitialInputs: initial}
	h.sessions[o.ID] = s
	go func() {
		runErr := coordinator.New(dependencies).Run(cctx)
		s.mu.Lock()
		if runErr == nil {
			runErr = s.completeStops(context.WithoutCancel(cctx))
		}
		s.mu.Unlock()
		cancel()
		if s.runtime.Close != nil {
			runErr = errors.Join(runErr, s.runtime.Close())
		}
		if manager, ok := s.runtime.Operations.(interface{ Done() <-chan struct{} }); ok {
			<-manager.Done()
		}
		// No writer can be handed over until all canonical writes have stopped.
		runErr = errors.Join(runErr, lock.Close())
		s.mu.Lock()
		s.running = false
		s.progress = nil
		s.err = runErr
		s.broadcast(Event{Kind: "stopped"})
		for id, ch := range s.subscribers {
			close(ch)
			delete(s.subscribers, id)
		}
		close(s.done)
		s.mu.Unlock()
	}()
	return s, nil
}
func (h *Host) Create(ctx context.Context, o Options) (*Session, error) {
	o.Mode = Create
	return h.Open(ctx, o)
}
func (h *Host) Resume(ctx context.Context, o Options) (*Session, error) {
	o.Mode = Resume
	return h.Open(ctx, o)
}
func (h *Host) Attach(id session.ID) (*Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	if s == nil {
		return nil, fs.ErrNotExist
	}
	return s, nil
}
func (h *Host) Close() error {
	h.mu.Lock()
	h.closed = true
	h.cancel()
	all := make([]*Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		all = append(all, s)
	}
	h.mu.Unlock()
	for _, s := range all {
		<-s.done
	}
	return nil
}
func (s *Session) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	}
}
func (s *Session) Done() <-chan struct{} { return s.done }
func canonical(input inbox.Input) (inbox.Input, error) {
	if err := input.Validate(); err != nil {
		return inbox.Input{}, err
	}
	if input.Kind == inbox.InputExternal {
		var text string
		if err := json.Unmarshal(input.Payload, &text); err != nil {
			return inbox.Input{}, fmt.Errorf("external input must be a JSON string: %w", err)
		}
	}
	input.Payload = input.Payload.Clone()
	if len(input.Payload) > 0 {
		if err := input.Payload.Canonicalize(); err != nil {
			return inbox.Input{}, err
		}
	}
	return input, nil
}
func same(a, b inbox.Input) bool { return a.Kind == b.Kind && bytes.Equal(a.Payload, b.Payload) }
func (s *Session) register(input inbox.Input) (*submission, bool, error) {
	input, err := canonical(input)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	if old := s.submissions[input.ID]; old != nil {
		s.mu.Unlock()
		if !same(old.input, input) {
			return nil, false, ErrConflict
		}
		return old, false, nil
	}
	if !s.running {
		s.mu.Unlock()
		return nil, false, ErrStopped
	}
	p := &submission{input: input, done: make(chan struct{})}
	s.submissions[input.ID] = p
	s.mu.Unlock()
	return p, true, nil
}
func (s *Session) enqueue(input inbox.Input) (*submission, error) {
	p, fresh, err := s.register(input)
	if err != nil || !fresh {
		return p, err
	}
	input = p.input
	if err = s.inbox.Submit(s.ctx, input); err != nil {
		s.mu.Lock()
		p.err = err
		close(p.done)
		s.mu.Unlock()
		return nil, err
	}
	return p, nil
}
func (s *Session) Submit(ctx context.Context, generation string, input inbox.Input) (Receipt, error) {
	if generation != s.Generation {
		return Receipt{}, ErrStaleGeneration
	}
	if err := context.Cause(ctx); err != nil {
		return Receipt{}, err
	}
	p, err := s.enqueue(input)
	if err != nil {
		return Receipt{}, err
	}
	select {
	case <-p.done:
		return p.receipt, p.err
	case <-ctx.Done():
		return Receipt{}, context.Cause(ctx)
	case <-s.done:
		select {
		case <-p.done:
			return p.receipt, p.err
		default:
			return Receipt{}, ErrStopped
		}
	}
}
func (s *Session) Stop(ctx context.Context, generation string, mode inbox.ControlMode, reason string) (Receipt, error) {
	if mode != inbox.StopHard && mode != inbox.StopWhenIdle {
		return Receipt{}, fmt.Errorf("invalid stop mode")
	}
	payload, _ := json.Marshal(inbox.ControlMessage{Mode: mode, Reason: reason})
	return s.Submit(ctx, generation, inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload})
}
func (s *Session) Inspect(after sessionstore.Sequence, limit int) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(after, limit)
}
func (s *Session) view(after sessionstore.Sequence, limit int) (View, error) {
	if limit <= 0 || limit > 4096 {
		return View{}, fmt.Errorf("page limit must be 1..4096")
	}
	if after > sessionstore.Sequence(len(s.items)) {
		return View{}, fmt.Errorf("history cursor beyond session")
	}
	end := min(int(after)+limit, len(s.items))
	v := View{Progress: s.progress, ProjectInstructions: s.instructions, Session: s.snapshot, Generation: s.Generation, Revision: s.revision, Running: s.running,
		History: sessionstore.Page{Items: append([]sessionstore.Item(nil), s.items[int(after):end]...), NextAfter: sessionstore.Sequence(end), More: end < len(s.items)}}
	if s.err != nil {
		v.Failure = s.err.Error()
	}
	for _, op := range s.operations {
		v.Operations = append(v.Operations, op)
	}
	sort.Slice(v.Operations, func(i, j int) bool { return v.Operations[i].ID < v.Operations[j].ID })
	return clone(v), nil
}

// Subscribe atomically installs a subscriber and captures a snapshot. A gap
// closes that subscription: fetch a new snapshot and subscribe again.
func (s *Session) Subscribe(after sessionstore.Sequence, limit, capacity int) (Subscription, error) {
	if capacity < 1 || capacity > 4096 {
		return Subscription{}, fmt.Errorf("event capacity must be 1..4096")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.view(after, limit)
	if err != nil {
		return Subscription{}, err
	}
	ch := make(chan Event, capacity)
	s.nextSubscriber++
	id := s.nextSubscriber
	if s.running {
		s.subscribers[id] = ch
	} else {
		close(ch)
	}
	return Subscription{Initial: v, Events: ch, Cancel: func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if c, ok := s.subscribers[id]; ok {
			delete(s.subscribers, id)
			close(c)
		}
	}}, nil
}
func (s *Session) broadcast(event Event) {
	s.revision++
	event.Generation = s.Generation
	event.Revision = s.revision
	for id, ch := range s.subscribers {
		select {
		case ch <- clone(event):
		default:
			for len(ch) > 0 {
				<-ch
			}
			ch <- Event{Generation: s.Generation, Revision: s.revision, Kind: "gap"}
			close(ch)
			delete(s.subscribers, id)
		}
	}
}
func clone[T any](v T) T {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var result T
	if err = json.Unmarshal(data, &result); err != nil {
		panic(err)
	}
	return result
}
func (s *Session) rememberItem(item sessionstore.Item, notify bool) {
	item = clone(item)
	s.items = append(s.items, item)
	if item.Kind == sessionstore.ItemTurn || item.Kind == sessionstore.ItemModelResponse {
		s.progress = nil
	}
	if item.Kind == sessionstore.ItemInput {
		input, err := canonical(item.Data.(inbox.Input))
		if err != nil {
			panic(err)
		}
		p := s.submissions[input.ID]
		if p == nil {
			p = &submission{input: input, done: make(chan struct{})}
			s.submissions[input.ID] = p
		}
		if !p.committed {
			p.receipt = Receipt{ID: input.ID, Sequence: item.Sequence}
			p.committed = true
			close(p.done)
		}
	}
	if item.Kind == sessionstore.ItemHostRecord {
		if r := item.Data.(sessionstore.HostRecord); r.Kind == sessionstore.HostProjectInstructions && s.instructions == nil {
			metadata := r.ProjectInstructions.Metadata()
			s.instructions = &metadata
		}
	}
	if item.Kind == sessionstore.ItemToolCallStatus {
		for _, op := range item.Data.(sessionstore.ToolCallStatus).Operations {
			s.operations[op.ID] = op
		}
	}
	if notify {
		s.broadcast(Event{Kind: "item", Item: &item})
	}
}
func (s *Session) configure(ctx context.Context, config jsontext.Value) error {
	if len(config) == 0 {
		config = jsontext.Value("{}")
	}
	config = config.Clone()
	if err := config.Canonicalize(); err != nil {
		return err
	}
	for _, item := range s.items {
		if item.Kind == sessionstore.ItemHostRecord {
			r := item.Data.(sessionstore.HostRecord)
			if r.Kind == "configuration" {
				prior := r.Configuration.Clone()
				if err := prior.Canonicalize(); err != nil {
					return err
				}
				if !bytes.Equal(prior, config) {
					return fmt.Errorf("session runtime configuration mismatch")
				}
				return nil
			}
		}
	}
	return s.store.AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{Version: 1, Kind: "configuration", Configuration: config})
}
func (s *Session) pendingStops() []inbox.Input {
	pending := map[inbox.ID]inbox.Input{}
	for _, item := range s.items {
		if item.Kind == sessionstore.ItemHostRecord {
			r := item.Data.(sessionstore.HostRecord)
			if r.Kind == "stop_complete" {
				for _, id := range r.Inputs {
					delete(pending, id)
				}
			}
		}
		if item.Kind == sessionstore.ItemInput {
			input := item.Data.(inbox.Input)
			if input.Kind == inbox.InputControl {
				c, err := input.DecodeControlMessage()
				if err == nil && (c.Mode == inbox.StopHard || c.Mode == inbox.StopWhenIdle) {
					pending[input.ID] = input
				}
			}
		}
	}
	result := make([]inbox.Input, 0, len(pending))
	for _, v := range pending {
		result = append(result, v)
	}
	return result
}
func (s *Session) pendingStop() *inbox.ControlMessage {
	var result *inbox.ControlMessage
	for _, input := range s.pendingStops() {
		c, _ := input.DecodeControlMessage()
		if result == nil || c.Mode == inbox.StopHard {
			result = &c
		}
	}
	return result
}
func (s *Session) completeStops(ctx context.Context) error {
	pending := s.pendingStops()
	if len(pending) == 0 {
		return nil
	}
	ids := make([]inbox.ID, 0, len(pending))
	for _, input := range pending {
		ids = append(ids, input.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return s.store.AppendHostRecord(ctx, s.ID, sessionstore.HostRecord{Version: 1, Kind: "stop_complete", Inputs: ids})
}
