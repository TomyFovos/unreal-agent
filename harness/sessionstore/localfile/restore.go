package localfile

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

var (
	ErrRestoreViewChanged = errors.New("restore read view changed")
	ErrRestoreViewClosed  = errors.New("restore read view closed")
)

// RestoreView owns one decoded log only during restoration. It is never a
// persistent cache: Close, cancellation and any local append attempt release it.
// The owning Host holds the exclusive writer lease throughout restoration.
type RestoreView struct {
	mu               sync.Mutex
	store            *Store
	id               session.ID
	ctx              context.Context
	state            *storedState
	index            *restoreIndex
	committedSize    int64
	reason, closeErr error
	stop             func() bool
}

// OpenRestoreView reads one canonical snapshot under the owning Host writer lease.
func (s *Store) OpenRestoreView(ctx context.Context, id session.ID) (view *RestoreView, err error) {
	return s.openRestoreView(ctx, id, false)
}

// OpenCheckpointedRestoreView uses a derived write head for an interactive
// Session. A cache miss starts from the canonical header and builds a new index.
func (s *Store) OpenCheckpointedRestoreView(ctx context.Context, id session.ID) (*RestoreView, error) {
	return s.openRestoreView(ctx, id, true)
}

func (s *Store) openRestoreView(ctx context.Context, id session.ID, checkpoint bool) (*RestoreView, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	encoded, err := s.readRecord(id)
	if err != nil {
		return nil, fmt.Errorf("read session %q: %w", id, err)
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	var state storedState
	var committedSize, start int64
	var index *restoreIndex
	var hit bool
	if checkpoint {
		state, start, index, hit = s.readRestoreCheckpoint(id, encoded)
		if hit {
			state, committedSize, err = replayLog(encoded, state, start, index)
		}
	}
	if !hit || err != nil {
		start, state = 0, storedState{}
		if checkpoint {
			index = &restoreIndex{prefix: sha256.New()}
		}
		state, committedSize, err = replayLog(encoded, state, start, index)
	}
	if err != nil {
		return nil, fmt.Errorf("read session %q: %w", id, err)
	}
	if state.Snapshot.Session.ID != id {
		return nil, fmt.Errorf("restore session identity mismatch")
	}
	if index != nil {
		index.prefix.Write(encoded[start:committedSize])
	}
	v := &RestoreView{store: s, id: id, ctx: ctx, state: &state, index: index, committedSize: committedSize}
	return s.installRestoreView(v)
}

// OpenHistoryItems borrows the owning Host's already restored canonical items.
// The Host holds its Session mutex until Close, so appends cannot change this
// boundary. Raw Store writes retain the same invalidation behavior as disk views.
func (s *Store) OpenHistoryItems(ctx context.Context, id session.ID, items []sessionstore.Item) (sessionstore.HistoryView, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	v, err := s.installRestoreView(&RestoreView{store: s, id: id, ctx: ctx, state: &storedState{Items: items}})
	if err != nil {
		return nil, err
	}
	return &borrowedHistoryView{view: v}, nil
}

type borrowedHistoryView struct{ view *RestoreView }

func (v *borrowedHistoryView) Items(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (sessionstore.Page, error) {
	return v.view.Items(ctx, id, after, limit)
}
func (v *borrowedHistoryView) Close() error { return v.view.Close() }

func (s *Store) installRestoreView(v *RestoreView) (*RestoreView, error) {
	v.mu.Lock()
	if !s.restoreView.CompareAndSwap(nil, v) {
		v.mu.Unlock()
		return nil, fmt.Errorf("another restore read view is active")
	}
	v.stop = context.AfterFunc(v.ctx, func() { v.finish(context.Cause(v.ctx)) })
	v.mu.Unlock()
	return v, nil
}

// Take transfers the decoded history to its exclusive Host owner and closes
// this view. Seeding the ordinary write head avoids rereading the same log for
// the first activity/configuration append. No transcript cache survives here.
func (v *RestoreView) Take(ctx context.Context, id session.ID) ([]sessionstore.Item, []operation.Operation, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.checkLocked(ctx, id); err != nil {
		return nil, nil, err
	}
	head, err := cloneRestoreValue(v.state.sessionHead)
	if err != nil {
		return nil, nil, err
	}
	v.store.setCachedWriteState(id, cachedWriteState{head: head, committedSize: v.committedSize, restoreIndex: v.index})
	items, ops := v.state.Items, v.state.Operations
	return items, ops, v.finishLocked(ErrRestoreViewClosed)
}

func (v *RestoreView) checkLocked(ctx context.Context, id session.ID) error {
	if id != v.id {
		return fmt.Errorf("restore view belongs to another session")
	}
	if v.reason != nil {
		return v.reason
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := context.Cause(v.ctx); err != nil {
		return err
	}
	return nil
}

// Canonical decode has already validated these finite, acyclic value types.
// Clone their Go values rather than JSON re-encoding: opaque JSON/signature
// bytes, nil/empty distinctions and concrete observer types remain exact.
func cloneRestoreValue[T any](value T) (T, error) {
	return cloneRestoreReflect(reflect.ValueOf(value)).Interface().(T), nil
}
func cloneRestoreReflect(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.New(value.Type()).Elem()
		copy.Set(cloneRestoreReflect(value.Elem()))
		return copy
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.New(value.Type().Elem())
		copy.Elem().Set(cloneRestoreReflect(value.Elem()))
		return copy
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		if value.Type().Elem().Kind() == reflect.Uint8 {
			reflect.Copy(copy, value)
			return copy
		}
		for i := range value.Len() {
			copy.Index(i).Set(cloneRestoreReflect(value.Index(i)))
		}
		return copy
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		copy := reflect.MakeMapWithSize(value.Type(), value.Len())
		for it := value.MapRange(); it.Next(); {
			copy.SetMapIndex(cloneRestoreReflect(it.Key()), cloneRestoreReflect(it.Value()))
		}
		return copy
	case reflect.Struct:
		copy := reflect.New(value.Type()).Elem()
		copy.Set(value)
		for i := range value.NumField() {
			// time.Time's private, immutable internals are copied by value.
			if value.Type().Field(i).PkgPath == "" {
				copy.Field(i).Set(cloneRestoreReflect(value.Field(i)))
			}
		}
		return copy
	case reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return value
		}
		copy := reflect.New(value.Type()).Elem()
		for i := range value.Len() {
			copy.Index(i).Set(cloneRestoreReflect(value.Index(i)))
		}
		return copy
	default:
		return value
	}
}

func (v *RestoreView) Items(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (sessionstore.Page, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if limit <= 0 {
		return sessionstore.Page{}, fmt.Errorf("item page limit must be positive")
	}
	if err := v.checkLocked(ctx, id); err != nil {
		return sessionstore.Page{}, err
	}
	start := min(uint64(after), uint64(len(v.state.Items)))
	end := min(start+uint64(limit), uint64(len(v.state.Items)))
	items := make([]sessionstore.Item, 0, end-start)
	for _, item := range v.state.Items[start:end] {
		if err := context.Cause(ctx); err != nil {
			return sessionstore.Page{}, err
		}
		items = append(items, cloneHistoryItem(item))
	}
	next := after
	if len(items) > 0 {
		next = items[len(items)-1].Sequence
	}
	return sessionstore.Page{Items: items, NextAfter: next, More: end < uint64(len(v.state.Items))}, nil
}

// Responses and turns comprise most of a long history. Their common immutable
// fields can be copied by value; opaque provider bytes and slices stay detached.
func cloneHistoryItem(item sessionstore.Item) sessionstore.Item {
	switch value := item.Data.(type) {
	case session.Turn:
		value.BatchRecords = slices.Clone(value.BatchRecords)
		if value.ContextAdmission != nil {
			admission := *value.ContextAdmission
			value.ContextAdmission = &admission
		}
		item.Data = value
		return item
	case sessionstore.ModelResponse:
		r := &value.Response
		r.Usage.Raw = r.Usage.Raw.Clone()
		r.Usage.Unknown = slices.Clone(r.Usage.Unknown)
		if r.Failure != nil {
			failure := *r.Failure
			r.Failure = &failure
		}
		if r.Generation != nil {
			generation := *r.Generation
			r.Generation = &generation
		}
		r.Output = slices.Clone(r.Output)
		for i, output := range r.Output {
			r.Output[i].ToolObservations = slices.Clone(output.ToolObservations)
			switch output.Data.(type) {
			case llm.Message, llm.ToolCall:
				if output.Responses == nil {
					continue
				}
			}
			r.Output[i], _ = cloneRestoreValue(output)
		}
		item.Data = value
		return item
	default:
		copy, _ := cloneRestoreValue(item)
		return copy
	}
}
func (v *RestoreView) Resume(ctx context.Context, id session.ID) (sessionstore.ResumeState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.checkLocked(ctx, id); err != nil {
		return sessionstore.ResumeState{}, err
	}
	value, err := cloneRestoreValue(v.state.resume())
	if err != nil {
		return value, err
	}
	return value, nil
}
func (v *RestoreView) Operations(ctx context.Context, id session.ID) ([]operation.Operation, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.checkLocked(ctx, id); err != nil {
		return nil, err
	}
	values, err := cloneRestoreValue(v.state.Operations)
	if err != nil {
		return nil, err
	}
	return values, nil
}

func (v *RestoreView) finish(reason error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.finishLocked(reason)
}
func (v *RestoreView) finishLocked(reason error) error {
	if v.reason != nil {
		return v.closeErr
	}
	v.reason = reason
	v.state = nil
	v.index = nil
	v.store.restoreView.CompareAndSwap(v, nil)
	if v.stop != nil {
		v.stop()
		v.stop = nil
	}
	return v.closeErr
}
func (v *RestoreView) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.reason != nil {
		if errors.Is(v.reason, ErrRestoreViewClosed) {
			return v.closeErr
		}
		return errors.Join(v.reason, v.closeErr)
	}
	stale := v.checkLocked(v.ctx, v.id)
	return errors.Join(stale, v.finishLocked(ErrRestoreViewClosed))
}
func (s *Store) invalidateRestoreView(id session.ID) error {
	if v := s.restoreView.Load(); v != nil && v.id == id {
		return v.finish(ErrRestoreViewChanged)
	}
	return nil
}
