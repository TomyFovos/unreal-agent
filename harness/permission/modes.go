package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"
)

// ModesConfig opts in to human control. Maximum is an operator-reviewed upper
// bound, not a grant. Old configurations without this field retain their policy.
type ModesConfig struct {
	Enabled               bool
	Maximum               *Config
	ApprovalTimeoutMillis int
}

func (c ModesConfig) Validate() error {
	if c.Enabled && c.Maximum == nil {
		return errors.New("permission modes require an explicit maximum policy")
	}
	if c.ApprovalTimeoutMillis < 0 || c.ApprovalTimeoutMillis > 3600000 {
		return errors.New("invalid approval timeout")
	}
	return nil
}

type Mode string

const (
	ReadOnly   Mode = "read_only"
	Ask        Mode = "ask"
	FullAccess Mode = "full_access"
)

func (m Mode) Valid() bool { return m == ReadOnly || m == Ask || m == FullAccess }
func (m Mode) Label() string {
	switch m {
	case ReadOnly:
		return "Read Only"
	case Ask:
		return "Ask for Approval"
	case FullAccess:
		return "Full Access"
	}
	return "Configured policy"
}

// Decision is canonical evidence, not a transferable permission credential.
type Decision struct {
	Mode  Mode
	State string // pending, read_only, allow_once, full_access, denied, timeout, canceled, revoked
}
type Request struct {
	ID, Tool, Scope string
	Privileged      bool
	// Orchestration starts or steers a child agent, which applies the mode to
	// itself: it holds the maximum, and mode changes reach the child instead
	// of revoking this.
	Orchestration bool
	// ReadOnlyCommand is a shell command that only reads (ReadOnlyCommand):
	// it runs in every mode, as Codex runs its known-safe commands, with the
	// maximum's permission to start the process.
	ReadOnlyCommand bool
}
type Pending struct {
	ID, Tool, Scope string
	Deadline        time.Time
}
type ModeSnapshot struct {
	Enabled bool
	Mode    Mode
	// Stopped distinguishes selected historical mode from effective authority.
	// Omitted false preserves compatibility with live old-client snapshots.
	Stopped      bool
	SelectedMode Mode
	Revision     uint64
	Pending      []Pending
}
type Permit struct {
	Context  context.Context
	Decision Decision
	Denial   *Error
	Release  func()
}
type pendingApproval struct {
	request  Request
	ctx      context.Context
	result   chan Permit
	deadline time.Time
	stop     func() bool
}

// Controller is one Host-owned Session lifetime. Neither durable records nor
// provider input can recreate its approvals. All authority is memory-only.
type Controller struct {
	mu                sync.Mutex
	ctx               context.Context
	cancel            context.CancelFunc
	baseline, maximum *Policy
	mode              Mode
	revision          uint64
	timeout           time.Duration
	pending           map[string]*pendingApproval
	active            map[string]context.CancelFunc
	changed           chan struct{}
	lease             string
	challenge         string
	challengeRevision uint64
	stopped           bool
}
type controllerKey struct{}

func NewController(ctx context.Context, baseline, maximum *Policy, timeout time.Duration) *Controller {
	ctx, cancel := context.WithCancel(ctx)
	if baseline == nil {
		baseline = DenyAll()
	}
	baseline = baseline.Intersect(&Policy{all: true, readOnly: true})
	if maximum == nil {
		maximum = DenyAll()
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	c := &Controller{ctx: ctx, cancel: cancel, baseline: baseline, maximum: maximum, mode: ReadOnly, revision: 1, timeout: timeout, pending: map[string]*pendingApproval{}, active: map[string]context.CancelFunc{}, changed: make(chan struct{})}
	context.AfterFunc(ctx, func() { c.mu.Lock(); defer c.mu.Unlock(); c.stopLocked() })
	return c
}
func (c *Controller) Close() {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}
func (c *Controller) stopLocked() {
	if c.stopped {
		return
	}
	c.stopped = true
	c.revokeLocked("canceled")
	c.lease, c.challenge = "", ""
	c.notifyLocked()
}
func WithController(ctx context.Context, c *Controller) context.Context {
	ctx = WithPolicy(ctx, &Policy{controller: c})
	return context.WithValue(ctx, controllerKey{}, c)
}
func ControllerFromContext(ctx context.Context) *Controller {
	c, _ := ctx.Value(controllerKey{}).(*Controller)
	return c
}
func (c *Controller) currentPolicy() *Policy {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return DenyAll()
	}
	if c.mode == FullAccess {
		return c.maximum
	}
	return c.baseline.Intersect(c.maximum)
}

// bind replaces only this controller's leaf, preserving every immutable parent
// policy. An operation lease checks cancellation at each concrete I/O boundary.
func (p *Policy) bind(c *Controller, grant *Policy, ctx context.Context) *Policy {
	if p == nil {
		return DenyAll()
	}
	if p.controller == c {
		return &Policy{parents: []*Policy{grant, c.maximum}, guard: func() error {
			if ctx.Err() != nil {
				return deny("approval", "operation authorization revoked")
			}
			return nil
		}}
	}
	if len(p.parents) == 0 {
		return p
	}
	copy := *p
	copy.parents = make([]*Policy, len(p.parents))
	for i, parent := range p.parents {
		copy.parents[i] = parent.bind(c, grant, ctx)
	}
	return &copy
}
func (c *Controller) notifyLocked()          { c.revision++; close(c.changed); c.changed = make(chan struct{}) }
func (c *Controller) Snapshot() ModeSnapshot { s, _ := c.Watch(); return s }
func (c *Controller) Watch() (ModeSnapshot, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := ModeSnapshot{Enabled: true, Mode: c.mode, SelectedMode: c.mode, Stopped: c.ctx.Err() != nil, Revision: c.revision}
	if s.Stopped {
		s.Mode = ReadOnly
	}
	for _, p := range c.pending {
		s.Pending = append(s.Pending, Pending{ID: p.request.ID, Tool: p.request.Tool, Scope: p.request.Scope, Deadline: p.deadline})
	}
	sort.Slice(s.Pending, func(i, j int) bool { return s.Pending[i].ID < s.Pending[j].ID })
	return s, c.changed
}
func (c *Controller) denied(mode Mode, state string) Permit {
	return Permit{Decision: Decision{Mode: mode, State: state}, Denial: &Error{Denied, "approval", "human authorization " + state}, Release: func() {}}
}
func (c *Controller) permitLocked(ctx context.Context, r Request, state string, grant *Policy) Permit {
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	if (r.Privileged || c.mode == FullAccess) && !r.Orchestration && !r.ReadOnlyCommand {
		c.active[r.ID] = cancel
	}
	child = context.WithValue(child, contextKey{}, FromContext(ctx).bind(c, grant, child))
	var once sync.Once
	return Permit{Context: child, Decision: Decision{Mode: c.mode, State: state}, Release: func() { once.Do(func() { cancel(); stop(); c.mu.Lock(); delete(c.active, r.ID); c.mu.Unlock() }) }}
}

// Request returns immediately. The operation manager records pending evidence
// before observing this result; waiting never blocks its cancellation actor.
func (c *Controller) Request(ctx context.Context, r Request) <-chan Permit {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(chan Permit, 1)
	if c.ctx.Err() != nil || ctx.Err() != nil {
		result <- c.denied(c.mode, "canceled")
		return result
	}
	if r.ID == "" || r.Tool == "" || len(r.ID) > 256 || len(r.Tool) > 128 || len(r.Scope) > 512 {
		result <- c.denied(c.mode, "denied")
		return result
	}
	if _, ok := c.pending[r.ID]; ok {
		result <- c.denied(c.mode, "denied")
		return result
	}
	if _, ok := c.active[r.ID]; ok {
		result <- c.denied(c.mode, "denied")
		return result
	}
	if c.maximum.CheckTool(r.Tool) != nil {
		result <- c.denied(c.mode, "denied")
		return result
	}
	if !r.Privileged {
		grant := c.baseline
		state := "read_only"
		if c.mode == FullAccess {
			grant = c.maximum
			state = "full_access"
		}
		if r.Orchestration || r.ReadOnlyCommand {
			grant = c.maximum
		}
		result <- c.permitLocked(ctx, r, state, grant)
		return result
	}
	switch c.mode {
	case ReadOnly:
		result <- c.denied(c.mode, "read_only")
	case FullAccess:
		result <- c.permitLocked(ctx, r, "full_access", c.maximum)
	case Ask:
		if len(c.pending) >= 128 {
			result <- c.denied(Ask, "denied")
			return result
		}
		p := &pendingApproval{request: r, ctx: ctx, result: result, deadline: time.Now().Add(c.timeout)}
		c.pending[r.ID] = p
		waitCtx, stop := context.WithTimeout(ctx, c.timeout)
		p.stop = context.AfterFunc(waitCtx, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.pending[r.ID] != p {
				return
			}
			state := "canceled"
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				state = "timeout"
			}
			c.finishLocked(p, c.denied(Ask, state))
		})
		oldStop := p.stop
		p.stop = func() bool { ok := oldStop(); stop(); return ok }
		c.notifyLocked()
	default:
		result <- c.denied(c.mode, "denied")
	}
	return result
}
func (c *Controller) finishLocked(p *pendingApproval, result Permit) {
	delete(c.pending, p.request.ID)
	p.stop()
	p.result <- result
	c.notifyLocked()
}
func (c *Controller) revokeLocked(state string) {
	for _, p := range c.pending {
		c.finishLocked(p, c.denied(c.mode, state))
	}
	for _, cancel := range c.active {
		cancel()
	}
	clear(c.active)
	c.challenge = ""
}
func nonce() string {
	var v [32]byte
	if _, err := rand.Read(v[:]); err != nil {
		panic("cannot create permission control nonce")
	}
	return hex.EncodeToString(v[:])
}

var ErrControl = errors.New("permission control stale or unauthorized")

// AcquireControl is called only by the authenticated local TUI control stream.
// The token/challenge never enter projections, model context or durable history.
func (c *Controller) AcquireControl() (string, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := nonce()
	c.lease = key
	c.challenge = ""
	c.notifyLocked()
	var once sync.Once
	return key, func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.lease != key {
				return
			}
			c.lease = ""
			c.challenge = ""
			for _, p := range c.pending {
				c.finishLocked(p, c.denied(c.mode, "denied"))
			}
			c.notifyLocked()
		})
	}
}
func (c *Controller) validControl(key string, rev uint64) bool {
	return c.ctx.Err() == nil && key != "" && key == c.lease && rev == c.revision
}
func (c *Controller) PrepareFull(key string, rev uint64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.validControl(key, rev) {
		return "", ErrControl
	}
	c.challenge = nonce()
	c.challengeRevision = rev
	return c.challenge, nil
}
func (c *Controller) SetMode(key string, rev uint64, mode Mode, challenge string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.validControl(key, rev) || !mode.Valid() {
		return ErrControl
	}
	if mode == FullAccess && (challenge == "" || challenge != c.challenge || rev != c.challengeRevision) {
		return ErrControl
	}
	c.revokeLocked("revoked")
	c.mode = mode
	c.notifyLocked()
	return nil
}
func (c *Controller) Decide(key string, rev uint64, id string, allow bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.validControl(key, rev) || c.mode != Ask {
		return ErrControl
	}
	p := c.pending[id]
	if p == nil {
		return ErrControl
	}
	if !time.Now().Before(p.deadline) || p.ctx.Err() != nil {
		c.finishLocked(p, c.denied(Ask, "timeout"))
		return ErrControl
	}
	result := c.denied(Ask, "denied")
	if allow {
		result = c.permitLocked(p.ctx, p.request, "allow_once", c.maximum)
	}
	c.finishLocked(p, result)
	return nil
}

// Follow sets a child Session's mode from its parent agent, which holds the
// human's choice; a child has no human control stream. Like SetMode, a change
// ends active and pending authority.
func (c *Controller) Follow(mode Mode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil || !mode.Valid() {
		return ErrControl
	}
	if c.mode == mode {
		return nil
	}
	c.revokeLocked("revoked")
	c.mode = mode
	c.notifyLocked()
	return nil
}

// Answer decides a child Session's pending request for its parent agent, which
// approves child Operations instead of a human (U17).
func (c *Controller) Answer(id string, allow bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.pending[id]
	if c.ctx.Err() != nil || c.mode != Ask || p == nil {
		return ErrControl
	}
	if !time.Now().Before(p.deadline) || p.ctx.Err() != nil {
		c.finishLocked(p, c.denied(Ask, "timeout"))
		return ErrControl
	}
	result := c.denied(Ask, "denied")
	if allow {
		result = c.permitLocked(p.ctx, p.request, "allow_once", c.maximum)
	}
	c.finishLocked(p, result)
	return nil
}

// Read-only tools are a closed set. New/custom tools require approval by default.
// This does not grant any tool missing from the operator's baseline/ceiling.
func ReadOnlyTool(name string) bool {
	return slices.Contains([]string{"read", "grep", "glob", "ast_grep", "ViewImage", "SkillUse"}, name)
}

// MaximumContext is preflight-only. It retains immutable parent restrictions;
// the manager must obtain a Permit before passing any context to an executor.
func (c *Controller) MaximumContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, FromContext(ctx).bind(c, c.maximum, ctx))
}
