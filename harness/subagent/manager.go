package subagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Config struct {
	Owner            *host.Session
	Directory        string
	Binary           string
	Arguments        []string
	Environment      []string
	Templates        map[string]Template
	Child            *ChildConfig
	SendParent       Sender
	ValidateTemplate func(Template, Template) error
	ResolveRuntime   RuntimeResolver
}
type job struct {
	ctx        context.Context
	operation  operation.Operation
	done       chan struct{}
	cancel     chan struct{}
	once       sync.Once
	connection *processChannel
	approvals  []Approval // the child's requests waiting for this parent
}
type Manager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	config   Config
	mu       sync.Mutex
	jobs     map[operation.ID]*job
	canceled map[operation.ID]bool
	changed  chan struct{}
	updates  chan operation.Operation
	closed   bool
	workers  sync.WaitGroup
}

func NewManager(ctx context.Context, c Config) (*Manager, error) {
	if c.Owner == nil {
		return nil, fmt.Errorf("subagent owner is required")
	}
	if len(c.Templates) > 0 && (!filepath.IsAbs(c.Binary) || !filepath.IsAbs(c.Directory)) {
		return nil, fmt.Errorf("absolute child binary and store directory are required")
	}
	data, err := json.Marshal(c.Templates)
	if err != nil {
		return nil, err
	}
	c.Templates = nil
	if err = json.Unmarshal(data, &c.Templates); err != nil {
		return nil, err
	}
	c.Arguments = append([]string(nil), c.Arguments...)
	c.Environment = append([]string(nil), c.Environment...)
	if c.Child != nil {
		raw, _ := json.Marshal(c.Child)
		var copy ChildConfig
		_ = json.Unmarshal(raw, &copy)
		c.Child = &copy
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, config: c, jobs: map[operation.ID]*job{}, canceled: map[operation.ID]bool{}, changed: make(chan struct{}), updates: make(chan operation.Operation)}, nil
}
func (m *Manager) RemoteJobPlanType() operation.RemoteJobPlanType       { return PlanType }
func (m *Manager) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return PlanVersion }
func (m *Manager) RemoteJobUpdates() <-chan operation.Operation         { return m.updates }
func (m *Manager) AddRemoteJob(op operation.Operation) error {
	return m.AddRemoteJobWithContext(m.ctx, op)
}
func (m *Manager) AddRemoteJobWithContext(ctx context.Context, op operation.Operation) error {
	plan, err := DecodePlan(op)
	if err != nil {
		return err
	}
	if plan.ParentID != m.config.Owner.ID {
		return fmt.Errorf("subagent plan belongs to another parent")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return host.ErrStopped
	}
	if _, ok := m.jobs[op.ID]; ok {
		return nil
	}
	j := &job{ctx: ctx, operation: op, cancel: make(chan struct{}), done: make(chan struct{})}
	if m.canceled[op.ID] || op.Status == operation.StatusCanceling {
		j.once.Do(func() { close(j.cancel) })
	}
	m.jobs[op.ID] = j
	m.notify()
	m.workers.Go(func() { m.run(j, plan) })
	return nil
}
func (m *Manager) notify() { close(m.changed); m.changed = make(chan struct{}) }
func (m *Manager) CancelRemoteJob(id operation.ID, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.canceled[id] = true
	if j := m.jobs[id]; j != nil {
		j.once.Do(func() { close(j.cancel) })
	}
	return nil
}

// Close self-cancels and drains children. It never waits on the owning Session.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.workers.Wait()
		return nil
	}
	m.closed = true
	m.cancel()
	m.notify()
	m.mu.Unlock()
	m.workers.Wait()
	return nil
}
func (m *Manager) emit(op operation.Operation) {
	select {
	case m.updates <- op:
	case <-m.ctx.Done():
	}
}
func (m *Manager) run(j *job, p Plan) {
	defer func() { close(j.done); m.mu.Lock(); m.notify(); m.mu.Unlock() }()
	ctx, cancel := context.WithCancel(j.ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	defer cancel()
	go func() {
		select {
		case <-j.cancel:
			cancel()
		case <-ctx.Done():
		}
	}()
	op := j.operation
	state, _ := operation.DecodeRemoteJobState(op)
	status := operation.StatusCompleted
	var err error
	select {
	case <-j.cancel:
		status = operation.StatusCanceled
	default:
		switch p.Action {
		case "start":
			status, err = m.spawn(ctx, j, p, &state)
		case "send":
			var connection *processChannel
			connection, err = m.connection(ctx, p.Handle)
			if err == nil {
				target := ChildID(p.ParentID, p.Handle)
				payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(p.ParentID), Target: string(target), Handle: string(p.Handle), Kind: "message", Text: p.Text})
				_, err = connection.send(ctx, inbox.Input{ID: inbox.ID("send:" + string(op.ID)), Kind: inbox.InputPeer, Payload: payload})
			}
		case "approve":
			err = m.approve(ctx, p)
			if err == nil {
				state.TerminalResult = "Denied the child's request " + p.Request
				if p.Allow {
					state.TerminalResult = "Allowed the child's request " + p.Request
				}
			}
		case "cancel":
			_, err = m.ownedChild(p.Handle)
			if err == nil {
				_, err = m.config.Owner.CancelOperation(ctx, m.config.Owner.Generation, inbox.ID("cancel-request:"+string(op.ID)), p.Handle)
			}
		case "parent":
			if m.config.Child == nil || m.config.SendParent == nil {
				err = fmt.Errorf("parent channel unavailable")
			} else {
				c := m.config.Child
				payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(c.ChildID), Target: string(c.ParentID), Handle: string(c.OperationID), Kind: "message", Text: p.Text})
				_, err = m.config.SendParent(ctx, inbox.Input{ID: inbox.ID("send:" + string(op.ID)), Kind: inbox.InputPeer, Payload: payload})
			}
		case "finish":
			err = m.config.Owner.CommitFinish(ctx, op.ID, *p.Result)
		}
	}
	select {
	case <-j.cancel:
		status = operation.StatusCanceled
	default:
	}
	if ctx.Err() != nil {
		status = operation.StatusCanceled
	}
	if err != nil && status != operation.StatusCanceled {
		status = operation.StatusFailed
		// The reason reaches the parent's model and the subagent command;
		// without it neither can tell what to fix.
		state.TerminalError = "subagent " + p.Action + " failed: " + clipError(err.Error())
		if errors.As(err, new(childStopped)) {
			state.TerminalError = "the child stopped with an error: " + clipError(err.Error())
		}
		if denial := permission.Failure(err); denial != nil {
			op.Denial = denial
			state.TerminalError = denial.Error()
		}
	} else if status == operation.StatusFailed && state.TerminalError == "" {
		state.TerminalError = "Child reported failed completion"
	} else if status == operation.StatusCompleted {
		if state.TerminalResult == "" {
			state.TerminalResult = "Subagent " + p.Action + " acknowledged"
		}
	}
	if status == operation.StatusCanceled {
		state.TerminalResult = ""
		state.TerminalError = ""
	}
	step, updateErr := operation.UpdateRemoteJob(op, state, status)
	if updateErr == nil {
		m.emit(*step.Operation)
	}
}
func (m *Manager) ownedChild(id operation.ID) (Plan, error) {
	view, err := m.config.Owner.Inspect(0, 1)
	if err != nil {
		return Plan{}, err
	}
	for _, op := range view.Operations {
		if op.ID != id {
			continue
		}
		p, err := DecodePlan(op)
		if err != nil || p.Action != "start" || p.ParentID != m.config.Owner.ID {
			return Plan{}, fmt.Errorf("child ownership mismatch")
		}
		return p, nil
	}
	return Plan{}, fmt.Errorf("child operation not owned")
}

// approve answers a request the child still waits on (U17).
func (m *Manager) approve(ctx context.Context, p Plan) error {
	connection, err := m.connection(ctx, p.Handle)
	if err != nil {
		return err
	}
	m.mu.Lock()
	waiting := false
	if j := m.jobs[p.Handle]; j != nil {
		for _, a := range j.approvals {
			waiting = waiting || a.ID == p.Request
		}
	}
	m.mu.Unlock()
	if !waiting {
		return fmt.Errorf("the child is not waiting for request %q", p.Request)
	}
	f := connection.endpoint.envelope("decide")
	f.Approval = &Approval{ID: p.Request, Allow: p.Allow}
	return connection.write(ctx, f)
}

func (m *Manager) connection(ctx context.Context, id operation.ID) (*processChannel, error) {
	if _, err := m.ownedChild(id); err != nil {
		return nil, err
	}
	for {
		m.mu.Lock()
		j := m.jobs[id]
		changed := m.changed
		var p *processChannel
		if j != nil {
			p = j.connection
		}
		closed := m.closed
		m.mu.Unlock()
		if p != nil {
			return p, nil
		}
		if closed {
			return nil, host.ErrStopped
		}
		if j != nil {
			select {
			case <-j.done:
				return nil, fmt.Errorf("child is no longer running")
			case <-j.cancel:
				return nil, fmt.Errorf("child canceled")
			default:
			}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}
func (m *Manager) spawn(ctx context.Context, j *job, p Plan, state *operation.RemoteJobState) (operation.Status, error) {
	allowed, ok := m.config.Templates[p.Template]
	actual, _ := json.Marshal(p.Configuration)
	expected, _ := json.Marshal(allowed)
	if !ok {
		return operation.StatusFailed, fmt.Errorf("child template changed")
	}
	if p.RuntimeBound && m.config.ValidateTemplate != nil {
		if err := m.config.ValidateTemplate(allowed, *p.Configuration); err != nil {
			return operation.StatusFailed, err
		}
		allowed = *p.Configuration
	} else if !sameJSON(actual, expected) {
		return operation.StatusFailed, fmt.Errorf("child template changed")
	}
	snapshot := m.config.Owner.BoundProjectInstructions()
	child := ChildConfig{MutationStateDirectory: allowed.MutationStateDirectory, ProjectInstructions: snapshot, Version: 1, ParentID: p.ParentID, ChildID: p.ChildID, OperationID: j.operation.ID, SessionDirectory: m.config.Directory, Workspace: allowed.Workspace, Runtime: allowed.Runtime, Policy: allowed.Policy, Baseline: allowed.Baseline, Subagents: allowed.Subagents, ReadyID: inbox.ID("ready:" + string(j.operation.ID)), Task: p.Text}
	if s := sessionstore.SelectionFromConfiguration(allowed.Runtime); s != nil && s.Provider == "claude-code" {
		child.TextOnly = !sessionstore.ToolBridgeEnabledFromConfiguration(child.Runtime)
		child.ProviderProcess = true
	}
	if err := child.Validate(); err != nil {
		return operation.StatusFailed, err
	}
	// With permission modes the child follows this Session's mode (D1, U17),
	// so its start holds the operator maximum (permission.Request.Orchestration),
	// which bounds the child process and its template.
	policy := permission.FromContext(ctx)
	if err := policy.CheckProcess(); err != nil {
		return operation.StatusFailed, err
	}
	control := permission.ControllerFromContext(ctx)
	var mode permission.Mode
	if control != nil {
		mode = control.Snapshot().Mode
	}
	for _, name := range child.Policy.Tools {
		if err := policy.CheckTool(name); err != nil {
			return operation.StatusFailed, err
		}
	}
	handle := Handle{Version: 1, ParentID: p.ParentID, ChildID: p.ChildID, OperationID: j.operation.ID}
	collect := func(requireTerminal bool) (bool, error) {
		view, err := ReadChild(ctx, m.config.Directory, p.ChildID, 0, 1)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		// Host creates the header (and optional instruction binding) before
		// configure commits lineage. A crash between these writes leaves a
		// legitimate, uninitialized child. Only that exact prefix may proceed
		// to Host.Open, which acquires the process-shared writer gate first.
		if view.Configuration == nil && view.Finish == nil && len(view.View.Operations) == 0 && !view.View.History.More {
			initializing := len(view.View.History.Items) == 0
			if len(view.View.History.Items) == 1 && child.ProjectInstructions != nil {
				if record, ok := view.View.History.Items[0].Data.(host.ProjectInstructionRecord); ok {
					got, _ := json.Marshal(record.ProjectInstructions)
					want, _ := json.Marshal(child.ProjectInstructions.Metadata())
					initializing = sameJSON(got, want)
				}
			}
			if initializing {
				return false, nil
			}
		}
		if view.Configuration == nil || view.Configuration.ParentID != p.ParentID || view.Configuration.OperationID != j.operation.ID {
			return false, fmt.Errorf("child canonical lineage mismatch")
		}
		// Config bytes bind provider/profile/workspace/policy across restarts.
		got, _ := json.Marshal(view.Configuration)
		want, _ := json.Marshal(child)
		if !sameJSON(got, want) {
			return false, fmt.Errorf("child canonical configuration mismatch")
		}
		if view.Finish == nil {
			return false, nil
		}
		if requireTerminal {
			// A live channel's finish notification is not execution authority.
			// Only a fully drained canonical child may be torn down early;
			// active/uncertain child work remains owned by its existing runtime.
			for _, op := range view.View.Operations {
				if op.Status != operation.StatusCompleted && op.Status != operation.StatusFailed && op.Status != operation.StatusCanceled {
					return false, nil
				}
			}
		}
		handle.Finish = view.Finish
		handle.Approvals = nil
		state.Handle, _ = json.Marshal(handle)
		state.TerminalResult = view.Finish.Result.Summary
		return true, nil
	}
	if done, err := collect(false); done || err != nil {
		return finishStatus(handle), err
	}
	state.Handle, _ = json.Marshal(handle)
	checkpoint, _ := operation.UpdateRemoteJob(j.operation, *state, operation.StatusAwaiting)
	m.emit(*checkpoint.Operation)
	select {
	case <-j.cancel:
		return operation.StatusCanceled, nil
	case <-ctx.Done():
		return operation.StatusCanceled, context.Cause(ctx)
	default:
	}
	channel := startProcess(ctx, m.config, child, mode)
	m.mu.Lock()
	j.connection = channel
	j.approvals = nil
	m.notify()
	m.mu.Unlock()
	defer func() { channel.cancel(); <-channel.done }()
	if control != nil {
		m.workers.Go(func() { followMode(ctx, channel, control, mode) })
	}
	var canceled bool
	var timer <-chan time.Time
	cancellation := j.cancel
	for {
		select {
		case <-ctx.Done():
			channel.cancel()
			return operation.StatusCanceled, context.Cause(ctx)
		case <-cancellation:
			canceled = true
			cancellation = nil
			timer = time.After(2 * time.Second)
			m.workers.Go(func() {
				ctx, cancel := context.WithTimeout(m.ctx, 2*time.Second)
				defer cancel()
				data, _ := json.Marshal(inbox.ControlMessage{Mode: inbox.StopHard, Reason: "parent canceled child"})
				_, _ = channel.send(ctx, inbox.Input{ID: inbox.ID("stop:" + string(j.operation.ID)), Kind: inbox.InputControl, Payload: data})
			})
		case <-timer:
			channel.cancel()
			timer = nil
		case event := <-channel.approvals:
			m.mu.Lock()
			j.approvals = slices.DeleteFunc(j.approvals, func(a Approval) bool { return a.ID == event.approval.ID })
			if !event.done {
				j.approvals = append(j.approvals, event.approval)
			}
			handle.Approvals = slices.Clone(j.approvals)
			m.notify()
			m.mu.Unlock()
			state.Handle, _ = json.Marshal(handle)
			update, _ := operation.UpdateRemoteJob(j.operation, *state, operation.StatusAwaiting)
			m.emit(*update.Operation)
			if !event.done {
				m.workers.Go(func() { m.askParent(ctx, j, child, channel, event.approval) })
			}
		case <-channel.finishHint:
			if !canceled {
				if done, err := collect(true); done || err != nil {
					// spawn's defer cancels and joins this exact child channel
					// before run can publish a terminal parent Operation.
					return finishStatus(handle), err
				}
			}
		case <-channel.done:
			if canceled {
				return operation.StatusCanceled, nil
			}
			if done, err := collect(false); done || err != nil {
				return finishStatus(handle), err
			}
			err := channel.err
			if err == nil {
				err = fmt.Errorf("child exited without canonical Finish")
			}
			select {
			case <-channel.connected:
				err = childStopped{err}
			default:
			}
			return operation.StatusFailed, err
		}
	}
}

// childStopped is a child that started and then ended with an error, which
// "start failed" would misreport.
type childStopped struct{ err error }

func (e childStopped) Error() string { return e.err.Error() }
func (e childStopped) Unwrap() error { return e.err }

// followMode keeps a live child in this Session's permission mode.
func followMode(ctx context.Context, channel *processChannel, control *permission.Controller, sent permission.Mode) {
	for {
		snapshot, changed := control.Watch()
		if !snapshot.Stopped && snapshot.Mode != sent {
			f := channel.endpoint.envelope("mode")
			f.Mode = snapshot.Mode
			if channel.write(ctx, f) != nil {
				return
			}
			sent = snapshot.Mode
		}
		select {
		case <-ctx.Done():
			return
		case <-channel.done:
			return
		case <-changed:
		}
	}
}

// askParent wakes this Session's model with a child's request (U17). It is
// a peer message: provenance for the model, not an instruction. A model call
// in flight may answer the request itself (Claude waits with the subagent
// command); a notice then would only start another turn after the answer.
// So an idle parent hears at once, a busy one when its call ends with the
// request still open.
func (m *Manager) askParent(ctx context.Context, j *job, child ChildConfig, channel *processChannel, a Approval) {
	for {
		m.mu.Lock()
		open := slices.ContainsFunc(j.approvals, func(p Approval) bool { return p.ID == a.ID })
		changed := m.changed
		m.mu.Unlock()
		if !open {
			return
		}
		busy, responded := m.config.Owner.ModelBusy()
		if !busy {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-responded:
		}
	}
	text := fmt.Sprintf("Child %s asks for approval of request %s: %s %s. Allow or deny it as its parent; it waits until you answer.", child.OperationID, a.ID, a.Tool, a.Scope)
	payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(child.ChildID), Target: string(child.ParentID), Handle: string(child.OperationID), Kind: "message", Text: text})
	sum := sha256.Sum256([]byte(string(child.OperationID) + "\x00" + channel.endpoint.connection + "\x00" + a.ID))
	input := inbox.Input{ID: inbox.ID("approval:" + hex.EncodeToString(sum[:16])), Kind: inbox.InputPeer, Payload: payload}
	_, _ = m.config.Owner.SubmitPeer(ctx, m.config.Owner.Generation, string(child.ChildID), string(child.OperationID), input)
}

func clipError(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	if len(text) > 2000 {
		cut := 2000
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

func sameJSON(a, b []byte) bool {
	x := jsontext.Value(a).Clone()
	y := jsontext.Value(b).Clone()
	return x.Canonicalize() == nil && y.Canonicalize() == nil && bytes.Equal(x, y)
}

func finishStatus(h Handle) operation.Status {
	if h.Finish != nil && h.Finish.Result.Status == "failed" {
		return operation.StatusFailed
	}
	return operation.StatusCompleted
}
