package subagent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

type Config struct {
	Owner       *host.Session
	Directory   string
	Binary      string
	Arguments   []string
	Environment []string
	Templates   map[string]Template
	Child       *ChildConfig
	SendParent  Sender
}
type job struct {
	operation  operation.Operation
	done       chan struct{}
	cancel     chan struct{}
	once       sync.Once
	connection *processChannel
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
	j := &job{operation: op, cancel: make(chan struct{}), done: make(chan struct{})}
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
	ctx, cancel := context.WithCancel(m.ctx)
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
			status, err = m.spawn(j, p, &state)
		case "send":
			var connection *processChannel
			connection, err = m.connection(ctx, p.Handle)
			if err == nil {
				target := ChildID(p.ParentID, p.Handle)
				payload, _ := json.Marshal(inbox.PeerMessage{Version: 1, Sender: string(p.ParentID), Target: string(target), Handle: string(p.Handle), Kind: "message", Text: p.Text})
				_, err = connection.send(ctx, inbox.Input{ID: inbox.ID("send:" + string(op.ID)), Kind: inbox.InputPeer, Payload: payload})
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
	if err != nil && status != operation.StatusCanceled {
		status = operation.StatusFailed
		state.TerminalError = "subagent " + p.Action + " failed"
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
func (m *Manager) spawn(j *job, p Plan, state *operation.RemoteJobState) (operation.Status, error) {
	allowed, ok := m.config.Templates[p.Template]
	actual, _ := json.Marshal(p.Configuration)
	expected, _ := json.Marshal(allowed)
	if !ok || !sameJSON(actual, expected) {
		return operation.StatusFailed, fmt.Errorf("child template changed")
	}
	snapshot := m.config.Owner.BoundProjectInstructions()
	child := ChildConfig{MutationStateDirectory: allowed.MutationStateDirectory, ProjectInstructions: snapshot, Version: 1, ParentID: p.ParentID, ChildID: p.ChildID, OperationID: j.operation.ID, SessionDirectory: m.config.Directory, Workspace: allowed.Workspace, Runtime: allowed.Runtime, Policy: allowed.Policy, ReadyID: inbox.ID("ready:" + string(j.operation.ID)), Task: p.Text}
	if err := child.Validate(); err != nil {
		return operation.StatusFailed, err
	}
	policy := permission.FromContext(m.ctx)
	if err := policy.CheckProcess(); err != nil {
		return operation.StatusFailed, err
	}
	for _, name := range child.Policy.Tools {
		if err := policy.CheckTool(name); err != nil {
			return operation.StatusFailed, err
		}
	}
	handle := Handle{Version: 1, ParentID: p.ParentID, ChildID: p.ChildID, OperationID: j.operation.ID}
	collect := func() (bool, error) {
		view, err := ReadChild(m.ctx, m.config.Directory, p.ChildID, 0, 1)
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
		handle.Finish = view.Finish
		state.Handle, _ = json.Marshal(handle)
		state.TerminalResult = view.Finish.Result.Summary
		return true, nil
	}
	if done, err := collect(); done || err != nil {
		return finishStatus(handle), err
	}
	state.Handle, _ = json.Marshal(handle)
	checkpoint, _ := operation.UpdateRemoteJob(j.operation, *state, operation.StatusAwaiting)
	m.emit(*checkpoint.Operation)
	select {
	case <-j.cancel:
		return operation.StatusCanceled, nil
	case <-m.ctx.Done():
		return operation.StatusCanceled, context.Cause(m.ctx)
	default:
	}
	channel := startProcess(m.ctx, m.config, child)
	m.mu.Lock()
	j.connection = channel
	m.notify()
	m.mu.Unlock()
	defer func() { channel.cancel(); <-channel.done }()
	var canceled bool
	var timer <-chan time.Time
	cancellation := j.cancel
	for {
		select {
		case <-m.ctx.Done():
			channel.cancel()
			return operation.StatusCanceled, context.Cause(m.ctx)
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
		case <-channel.done:
			if canceled {
				return operation.StatusCanceled, nil
			}
			if done, err := collect(); done || err != nil {
				return finishStatus(handle), err
			}
			if channel.err != nil {
				return operation.StatusFailed, channel.err
			}
			return operation.StatusFailed, fmt.Errorf("child exited without canonical Finish")
		}
	}
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
