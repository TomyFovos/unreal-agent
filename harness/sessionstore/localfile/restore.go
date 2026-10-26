package localfile

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"golang.org/x/sys/unix"
)

var (
	ErrRestoreViewChanged     = errors.New("restore read view changed")
	ErrRestoreViewClosed      = errors.New("restore read view closed")
	ErrRestoreViewUnsupported = errors.New("scoped restore read view unavailable")
)

type restoreIdentity struct {
	device, inode                                                      uint64
	size, modifiedSeconds, modifiedNanos, changedSeconds, changedNanos int64
}

// RestoreView owns one decoded log only during restoration. It is never a
// persistent cache: Close, cancellation and any local append attempt release it.
// The duplicated writer descriptor keeps the same exclusive writer lease alive;
// it cannot confer a parent's Operation rights or be reused by a new owner.
type RestoreView struct {
	mu               sync.Mutex
	store            *Store
	id               session.ID
	ctx              context.Context
	state            *storedState
	log, writer      *os.File
	identity         restoreIdentity
	version          int
	reason, closeErr error
	stop             func() bool
}

// OpenRestoreView requires the same sidecar writer lease used by Host.Open.
// File timestamps (including ctime), identity and length are checked between
// pages: an external same-size rewrite is not treated as an unchanged log.
func (s *Store) OpenRestoreView(ctx context.Context, id session.ID, owner *os.File) (view *RestoreView, err error) {
	if err = validateSessionID(id); err != nil {
		return nil, err
	}
	if err = context.Cause(ctx); err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, fmt.Errorf("restore requires a writer lease")
	}
	conn, err := owner.SyscallConn()
	if err != nil {
		return nil, err
	}
	duplicate := -1
	var duplicateErr error
	if err = conn.Control(func(fd uintptr) { duplicate, duplicateErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0) }); err != nil {
		return nil, err
	}
	if duplicateErr != nil {
		return nil, duplicateErr
	}
	lease := os.NewFile(uintptr(duplicate), "restore writer lease")
	defer func() {
		if err != nil {
			lease.Close()
		}
	}()
	actual, err := lease.Stat()
	if err != nil {
		return nil, err
	}
	expected, err := os.Lstat(s.sessionPath(id) + ".lock")
	if err != nil {
		return nil, err
	}
	if !actual.Mode().IsRegular() || !expected.Mode().IsRegular() || !os.SameFile(actual, expected) {
		return nil, fmt.Errorf("restore writer lease does not own this session")
	}
	if _, err = restoreIdentityOf(actual); err != nil {
		return nil, err
	}
	if err = unix.Flock(duplicate, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, ErrWriterOwned
	}
	fd, err := unix.Open(s.sessionPath(id), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("read session %q: %w", id, err)
	}
	log := os.NewFile(uintptr(fd), "restore canonical log")
	defer func() {
		if err != nil {
			log.Close()
		}
	}()
	info, err := log.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("restore canonical log is not a regular file")
	}
	stamp, err := restoreIdentityOf(info)
	if err != nil {
		return nil, err
	}
	encoded, err := io.ReadAll(log)
	if err != nil {
		return nil, err
	}
	if err = context.Cause(ctx); err != nil {
		return nil, err
	}
	state, _, err := decodeLog(encoded)
	if err != nil {
		return nil, fmt.Errorf("read session %q: %w", id, err)
	}
	if state.Snapshot.Session.ID != id {
		return nil, fmt.Errorf("restore session identity mismatch")
	}
	first, _, _ := bytes.Cut(encoded, []byte{'\n'})
	var record logRecord
	var header sessionRecord
	if err = json.Unmarshal(first, &record); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(record.Data, &header); err != nil {
		return nil, err
	}
	v := &RestoreView{store: s, id: id, ctx: ctx, state: &state, log: log, writer: lease, identity: stamp, version: header.Version}
	if err = v.checkLocked(ctx, id); err != nil {
		return nil, err
	}
	v.mu.Lock()
	if !s.restoreView.CompareAndSwap(nil, v) {
		v.mu.Unlock()
		return nil, fmt.Errorf("another restore read view is active")
	}
	v.stop = context.AfterFunc(ctx, func() { v.finish(context.Cause(ctx)) })
	v.mu.Unlock()
	return v, nil
}

// FormatVersion is immutable header metadata, not an execution grant.
func (v *RestoreView) FormatVersion() int { return v.version }

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
	info, err := v.log.Stat()
	if err != nil {
		return ErrRestoreViewChanged
	}
	stamp, err := restoreIdentityOf(info)
	if err != nil {
		return err
	}
	pathInfo, err := os.Lstat(v.store.sessionPath(v.id))
	if err != nil || !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		return ErrRestoreViewChanged
	}
	pathStamp, err := restoreIdentityOf(pathInfo)
	if err != nil {
		return err
	}
	if stamp != v.identity || pathStamp != v.identity {
		return ErrRestoreViewChanged
	}
	lockInfo, err := v.writer.Stat()
	if err != nil {
		return ErrRestoreViewChanged
	}
	currentLock, err := os.Lstat(v.store.sessionPath(v.id) + ".lock")
	if err != nil || !currentLock.Mode().IsRegular() || !os.SameFile(lockInfo, currentLock) {
		return ErrRestoreViewChanged
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
		copy, err := cloneRestoreValue(item)
		if err != nil {
			return sessionstore.Page{}, err
		}
		items = append(items, copy)
	}
	if err := v.checkLocked(ctx, id); err != nil {
		return sessionstore.Page{}, err
	}
	next := after
	if len(items) > 0 {
		next = items[len(items)-1].Sequence
	}
	return sessionstore.Page{Items: items, NextAfter: next, More: end < uint64(len(v.state.Items))}, nil
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
	if err = v.checkLocked(ctx, id); err != nil {
		return sessionstore.ResumeState{}, err
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
	if err = v.checkLocked(ctx, id); err != nil {
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
	v.store.restoreView.CompareAndSwap(v, nil)
	if v.stop != nil {
		v.stop()
		v.stop = nil
	}
	v.closeErr = errors.Join(v.log.Close(), v.writer.Close())
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
