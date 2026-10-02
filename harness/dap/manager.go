package dap

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

type Manager struct {
	ctx        context.Context
	cancel     context.CancelFunc
	generation string
	configs    map[string]AdapterConfig
	mu         sync.Mutex
	sessions   map[string]*liveSession
	jobs       map[operation.ID]context.CancelFunc
	seen       map[operation.ID]struct{}
	canceled   map[operation.ID]bool
	updates    chan operation.Operation
	workers    sync.WaitGroup
	closed     bool
	closedDone chan struct{}
}

func NewManager(ctx context.Context, configs ...AdapterConfig) (*Manager, error) {
	if !permission.Configured(ctx) {
		ctx = permission.WithPolicy(ctx, permission.DenyAll())
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{ctx: ctx, cancel: cancel, generation: uuid.New().String(), configs: map[string]AdapterConfig{}, sessions: map[string]*liveSession{}, jobs: map[operation.ID]context.CancelFunc{}, seen: map[operation.ID]struct{}{}, canceled: map[operation.ID]bool{}, updates: make(chan operation.Operation, 32), closedDone: make(chan struct{})}
	for _, c := range configs {
		if !idPattern.MatchString(c.ID) || !filepath.IsAbs(c.Path) || !filepath.IsAbs(c.Directory) {
			cancel()
			return nil, &Error{Code: "invalid_adapter_configuration"}
		}
		if _, exists := m.configs[c.ID]; exists {
			cancel()
			return nil, &Error{Code: "duplicate_adapter"}
		}
		for _, raw := range []jsontext.Value{c.LaunchFields, c.AttachFields} {
			if len(raw) > 0 {
				var fields map[string]jsontext.Value
				if len(raw) > MaxResultBytes || json.Unmarshal(raw, &fields) != nil || fields == nil {
					cancel()
					return nil, &Error{Code: "invalid_adapter_configuration"}
				}
			}
		}
		m.configs[c.ID] = copyConfig(c)
	}
	// Parent cancellation still performs bounded disconnect before killing the adapter.
	context.AfterFunc(ctx, func() { _ = m.Close() })
	return m, nil
}
func (m *Manager) Generation() string { return m.generation }
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.closedDone
		return nil
	}
	m.closed = true
	defer close(m.closedDone)
	sessions := make([]*liveSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	cancels := make([]context.CancelFunc, 0, len(m.jobs))
	for _, cancel := range m.jobs {
		cancels = append(cancels, cancel)
	}
	m.mu.Unlock()
	m.cancel() // release in-flight request locks; adapter processes remain alive for detach
	for _, cancel := range cancels {
		cancel()
	}
	for _, s := range sessions {
		s.shutdown()
	}
	m.workers.Wait()
	return nil
}
func (m *Manager) RemoteJobPlanType() operation.RemoteJobPlanType       { return PlanType }
func (m *Manager) RemoteJobPlanVersion() operation.RemoteJobPlanVersion { return PlanVersion }
func (m *Manager) RemoteJobUpdates() <-chan operation.Operation         { return m.updates }
func (m *Manager) AddRemoteJob(current operation.Operation) error {
	state, err := operation.DecodeRemoteJobState(current)
	if err != nil {
		return err
	}
	var request Request
	if state.Plan.Type != PlanType || state.Plan.Version != PlanVersion || json.Unmarshal(state.Plan.Data, &request, json.RejectUnknownMembers(true)) != nil {
		return operation.ErrUnsupported
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return context.Canceled
	}
	if _, ok := m.seen[current.ID]; ok {
		m.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	if m.canceled[current.ID] || current.Status == operation.StatusCanceling {
		cancel()
	}
	m.jobs[current.ID] = cancel
	m.seen[current.ID] = struct{}{}
	m.workers.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.workers.Done()
		defer cancel()
		defer func() { m.mu.Lock(); delete(m.jobs, current.ID); m.mu.Unlock() }()
		var result Result
		var err error
		// An operation from a previous owner is expired even if its last checkpoint
		// says Ready. Dispatch and persistence are not assumed to be a transaction.
		if request.OwnerGeneration != m.generation {
			result = Result{Version: 1, Handle: request.Handle, Command: request.Command, Status: "expired", Error: "expired"}
			err = &Error{Code: "expired"}
		} else {
			result, err = m.Execute(ctx, request)
		}
		if ctx.Err() != nil {
			result.Status = "interrupted"
			result.Error = "interrupted"
		}
		encoded, encodeErr := json.Marshal(result)
		if encodeErr != nil || len(encoded) > current.MaxOutputLength {
			result = Result{Version: 1, Handle: request.Handle, Command: request.Command, Status: "failed", Error: "result_too_large"}
			encoded, _ = json.Marshal(result)
			err = &Error{Code: "result_too_large"}
		}
		state.TerminalResult = string(encoded)
		state.TerminalError = ""
		state.Handle = nil
		status := operation.StatusCompleted
		if err != nil {
			status = operation.StatusFailed
			state.TerminalError = codeOf(err)
			if d := permission.Failure(err); d != nil {
				current.Denial = d
				state.TerminalError = "permission_" + string(d.Code)
			}
		}
		if ctx.Err() != nil {
			status = operation.StatusCanceled
		}
		// Preserve a valid typed outcome in Handle, independently of model-facing
		// text bounds. TerminalResult uses our bounded formatter and is never a
		// required canonical JSON field.
		state.Handle = encoded
		step, updateErr := operation.UpdateRemoteJob(current, state, status)
		if updateErr != nil {
			return
		}
		select {
		case m.updates <- *step.Operation:
		case <-m.ctx.Done():
		}
	}()
	return nil
}
func (m *Manager) CancelRemoteJob(id operation.ID, _ string) error {
	m.mu.Lock()
	m.canceled[id] = true
	cancel := m.jobs[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}
func (m *Manager) Execute(ctx context.Context, r Request) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	result := Result{Version: 1, Handle: r.Handle, Command: r.Command, Status: "failed"}
	fail := func(err error) (Result, error) {
		result.Error = codeOf(err)
		if d := permission.Failure(err); d != nil {
			result.Error = "permission_" + string(d.Code)
		}
		return result, err
	}
	if err := r.Validate(); err != nil {
		return fail(err)
	}
	if r.OwnerGeneration != m.generation {
		return fail(&Error{Code: "expired"})
	}
	if err := ctx.Err(); err != nil {
		return fail(&Error{Code: "interrupted"})
	}
	// Intersect caller restrictions with this Host's original authorization.
	policy := permission.FromContext(m.ctx).Intersect(permission.FromContext(ctx))
	if err := policy.CheckTool("DAP"); err != nil {
		return fail(err)
	}
	if err := policy.CheckProcess(); err != nil {
		return fail(err)
	}
	ctx = permission.WithPolicy(ctx, policy)
	if r.Command == "launch" || r.Command == "attach" {
		return m.start(ctx, r)
	}
	m.mu.Lock()
	s := m.sessions[r.Handle.ID]
	m.mu.Unlock()
	if s == nil || r.Handle.Generation != m.generation {
		return fail(&Error{Code: "expired"})
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	result = s.summary(r.Command)
	if result.Status == "expired" || result.Status == "terminated" {
		return fail(&Error{Code: "expired"})
	}
	if r.Command == "inspect" {
		return result, nil
	}
	s.mu.Lock()
	beforeEpoch := s.epoch
	if r.Command == "stackTrace" && s.state != "stopped" {
		s.mu.Unlock()
		return fail(&Error{Code: "not_stopped"})
	}
	if slices.Contains([]string{"scopes", "variables", "evaluate"}, r.Command) && (r.StopEpoch != s.epoch || s.state != "stopped") {
		s.mu.Unlock()
		return fail(&Error{Code: "stale_reference"})
	}
	if slices.Contains([]string{"continue", "next", "stepIn", "stepOut"}, r.Command) {
		s.state = "running"
		s.epoch++
	}
	s.mu.Unlock()
	if r.Command == "evaluate" {
		if err := policy.CheckTool("DAP.Evaluate"); err != nil {
			return fail(err)
		}
	}
	if r.Command == "setBreakpoints" {
		if err := policy.CheckPath(r.Source, false); err != nil {
			return fail(err)
		}
	}
	if r.Command == "disconnect" && r.Terminate && s.attached {
		return fail(&Error{Code: "cannot_terminate_attached_target"})
	}
	args := arguments(r)
	body, err := s.request(ctx, r.Command, args)
	if err != nil {
		result = s.summary(r.Command)
		return fail(err)
	}
	result = s.summary(r.Command)
	if slices.Contains([]string{"stackTrace", "scopes", "variables", "evaluate"}, r.Command) && result.StopEpoch != beforeEpoch {
		return fail(&Error{Code: "stale_reference"})
	}
	if r.Command == "disconnect" {
		s.mu.Lock()
		s.state = "terminated"
		s.mu.Unlock()
		result.Status = "terminated"
		s.cancel()
	}
	if err := decodeResult(&result, r, body); err != nil {
		return fail(err)
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > MaxResultBytes {
		return fail(&Error{Code: "result_too_large"})
	}
	return result, nil
}
func (m *Manager) start(ctx context.Context, r Request) (Result, error) {
	result := Result{Version: 1, Handle: Handle{ID: r.Handle.ID, Generation: m.generation}, Command: r.Command, Status: "failed"}
	fail := func(err error) (Result, error) {
		result.Error = codeOf(err)
		if d := permission.Failure(err); d != nil {
			result.Error = "permission_" + string(d.Code)
		}
		return result, err
	}
	c, ok := m.configs[r.Start.Adapter]
	if !ok {
		return fail(&Error{Code: "unsupported_adapter"})
	}
	if r.Command == "attach" && !slices.Contains(c.AllowedAttachPIDs, r.Start.ProcessID) {
		return fail(&permission.Error{Code: permission.Denied, Capability: "debuggee.attach", Reason: "target is not explicitly authorized"})
	}
	// Keep the adapter alive long enough to detach on owner cancellation. The
	// Manager cancellation callback owns shutdown and always cancels this context.
	processContext, cancel := context.WithCancel(context.WithoutCancel(m.ctx))
	s := &liveSession{owner: m.ctx, handle: result.Handle, attached: r.Command == "attach", cancel: cancel, done: make(chan struct{}), initialized: make(chan struct{}), pending: map[int]chan response{}, state: "starting"}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	m.mu.Lock()
	if m.closed || len(m.sessions) >= 8 {
		m.mu.Unlock()
		cancel()
		return fail(&Error{Code: "session_limit"})
	}
	if _, exists := m.sessions[r.Handle.ID]; exists {
		m.mu.Unlock()
		cancel()
		return fail(&Error{Code: "session_exists"})
	}
	m.sessions[r.Handle.ID] = s
	m.mu.Unlock()
	events := make(chan primitives.PrimitiveEvent, 32)
	s.process = primitives.StartProcess(processContext, primitives.ProcessStartRequest{Source: "dap", CorrelationID: primitives.CorrelationID(uuid.New().String()), Path: c.Path, Arguments: c.Arguments, Directory: c.Directory, Environment: c.Environment, Pipes: primitives.ProcessPipeAll}, events)
	go s.read(events)
	succeeded := false
	defer func() {
		if !succeeded {
			s.interrupt()
		}
	}()
	// Configuration is sent after initialized, while launch/attach is pending.
	body, err := s.request(ctx, "initialize", map[string]any{"clientID": "unreal-agent", "adapterID": c.ID, "pathFormat": "path", "linesStartAt1": true, "columnsStartAt1": true, "supportsVariablePaging": true, "supportsRunInTerminalRequest": false, "supportsStartDebuggingRequest": false})
	if err != nil {
		return fail(err)
	}
	var caps struct {
		ConfigurationDone bool `json:"supportsConfigurationDoneRequest"`
	}
	if len(body) > 0 && json.Unmarshal(body, &caps) != nil {
		return fail(&Error{Code: "invalid_capabilities"})
	}
	fields := map[string]any{}
	raw := c.LaunchFields
	if s.attached {
		raw = c.AttachFields
	}
	if len(raw) > 0 {
		if json.Unmarshal(raw, &fields) != nil {
			return fail(&Error{Code: "invalid_configuration"})
		}
	}
	fields["noDebug"] = false
	if s.attached {
		fields["processId"] = r.Start.ProcessID
	} else {
		fields["program"] = r.Start.Program
		fields["args"] = r.Start.Arguments
		fields["cwd"] = r.Start.Directory
		fields["stopOnEntry"] = r.Start.StopOnEntry
	}
	data, _ := json.Marshal(fields)
	pending, err := s.send(ctx, r.Command, data)
	if err != nil {
		return fail(err)
	}
	launchDone := false
	for {
		select {
		case <-s.initialized:
			goto initialized
		case response := <-pending:
			if response.err != nil {
				return fail(response.err)
			}
			launchDone = true
			pending = nil
		case <-ctx.Done():
			return fail(&Error{Code: "interrupted"})
		case <-s.done:
			return fail(&Error{Code: "expired"})
		}
	}
initialized:
	if caps.ConfigurationDone {
		if _, err := s.request(ctx, "configurationDone", map[string]any{}); err != nil {
			return fail(err)
		}
	}
	if !launchDone {
		select {
		case response := <-pending:
			if response.err != nil {
				return fail(response.err)
			}
		case <-ctx.Done():
			return fail(&Error{Code: "interrupted"})
		case <-s.done:
			return fail(&Error{Code: "expired"})
		}
	}
	s.mu.Lock()
	if s.state == "starting" {
		s.state = "running"
	}
	s.mu.Unlock()
	succeeded = true
	return s.summary(r.Command), nil
}
func arguments(r Request) map[string]any {
	count := r.Count
	if count == 0 {
		count = MaxItems
	}
	switch r.Command {
	case "continue", "pause", "next", "stepIn", "stepOut":
		return map[string]any{"threadId": r.ThreadID}
	case "stackTrace":
		return map[string]any{"threadId": r.ThreadID, "startFrame": r.Offset, "levels": count}
	case "scopes":
		return map[string]any{"frameId": r.FrameID}
	case "variables":
		return map[string]any{"variablesReference": r.VariablesReference, "start": r.Offset, "count": count}
	case "evaluate":
		return map[string]any{"expression": r.Expression, "frameId": r.FrameID, "context": "repl"}
	case "setBreakpoints":
		return map[string]any{"source": map[string]any{"path": r.Source}, "breakpoints": r.Breakpoints}
	case "disconnect":
		return map[string]any{"restart": false, "terminateDebuggee": r.Terminate}
	default:
		return map[string]any{}
	}
}
func decodeResult(out *Result, r Request, body jsontext.Value) error {
	if len(body) == 0 {
		return nil
	}
	type wireItem struct {
		ID                 int    `json:"id"`
		Name               string `json:"name"`
		Value              string `json:"value"`
		Type               string `json:"type"`
		VariablesReference int    `json:"variablesReference"`
		Line               int    `json:"line"`
		Column             int    `json:"column"`
		Verified           bool   `json:"verified"`
		Source             struct {
			Path string `json:"path"`
		} `json:"source"`
	}
	var data struct {
		Threads            []wireItem `json:"threads"`
		Frames             []wireItem `json:"stackFrames"`
		Scopes             []wireItem `json:"scopes"`
		Variables          []wireItem `json:"variables"`
		Breakpoints        []wireItem `json:"breakpoints"`
		Result             string     `json:"result"`
		VariablesReference int        `json:"variablesReference"`
	}
	if json.Unmarshal(body, &data) != nil {
		return &Error{Code: "invalid_response"}
	}
	var items []wireItem
	switch r.Command {
	case "threads":
		items = data.Threads
	case "stackTrace":
		items = data.Frames
	case "scopes":
		items = data.Scopes
	case "variables":
		items = data.Variables
	case "setBreakpoints":
		items = data.Breakpoints
	case "evaluate":
		out.Value = bounded(data.Result, &out.Truncated)
		out.VariablesReference = data.VariablesReference
	}
	count := r.Count
	if count == 0 {
		count = MaxItems
	}
	if len(items) > count {
		items = items[:count]
		out.Truncated = true
	}
	for _, i := range items {
		out.Items = append(out.Items, Item{ID: i.ID, Name: bounded(i.Name, &out.Truncated), Value: bounded(i.Value, &out.Truncated), Type: bounded(i.Type, &out.Truncated), VariablesReference: i.VariablesReference, Source: bounded(i.Source.Path, &out.Truncated), Line: i.Line, Column: i.Column, Verified: i.Verified})
	}
	return nil
}
func bounded(s string, truncated *bool) string {
	if len(s) <= 2048 {
		return s
	}
	s = s[:2048]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	*truncated = true
	return s + "…"
}

var _ operation.RemoteJobHandler = (*Manager)(nil)
