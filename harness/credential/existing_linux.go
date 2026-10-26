//go:build linux

package credential

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// existingLocal anchors an explicitly selected, already-private directory. It
// never creates a directory, lock, record or replacement credential. It has no
// login/refresh authority. Normal OpenLocal instances leave this pointer nil.
type existingLocal struct {
	mu        sync.Mutex
	directory *os.File
}

// OpenExistingLocal opens an existing store read-only. Linux descriptor-relative
// lookup rejects symlinks in every directory component. Renaming an ancestor
// after open cannot redirect this store to another directory. Credential record
// and lock descriptors are opened under the per-reference transaction lock.
func OpenExistingLocal(directory string) (*Local, error) {
	if directory == "" || strings.ContainsRune(directory, '\x00') {
		return nil, &Error{Code: "insecure_storage"}
	}
	for _, component := range strings.Split(directory, string(filepath.Separator)) {
		if component == ".." {
			return nil, &Error{Code: "insecure_storage"}
		}
	}
	path, err := filepath.Abs(directory)
	if err != nil {
		return nil, &Error{Code: "storage_unavailable"}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &Error{Code: "storage_unavailable"}
	}
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if openErr != nil {
			return nil, existingOpenError(openErr)
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), "existing-credential-store")
	if err := checkExistingDirectory(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Local{directory: path, existing: &existingLocal{directory: f}}, nil
}

// OpenExistingLocalAt binds the credential directory to an already validated
// dedicated owner root. It never re-resolves that root's pathname. The caller
// retains ownership of source; the returned store holds its own child fd.
func OpenExistingLocalAt(source *os.File, child string) (*Local, error) {
	if source == nil || child == "" || child == "." || child == ".." || strings.ContainsAny(child, "/\\\x00") {
		return nil, &Error{Code: "insecure_storage"}
	}
	if err := checkExistingDirectory(source); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(source.Fd()), child, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, existingOpenError(err)
	}
	f := os.NewFile(uintptr(fd), "existing-credential-store")
	if err := checkExistingDirectory(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Local{existing: &existingLocal{directory: f}}, nil
}

// Close releases the pinned store descriptor. It is idempotent and is a no-op
// for an ordinary OpenLocal store. An already-started transaction owns separate
// descriptors and remains governed by its original context and flock.
func (s *Local) Close() error {
	if s == nil || s.existing == nil {
		return nil
	}
	s.existing.mu.Lock()
	defer s.existing.mu.Unlock()
	if s.existing.directory == nil {
		return nil
	}
	err := s.existing.directory.Close()
	s.existing.directory = nil
	if err != nil {
		return &Error{Code: "storage_unavailable"}
	}
	return nil
}

func existingOpenError(err error) error {
	if errors.Is(err, unix.ENOENT) {
		return &Error{Code: "not_found"}
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
		return &Error{Code: "insecure_storage"}
	}
	return &Error{Code: "storage_unavailable"}
}

func checkExistingDirectory(f *os.File) error {
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || !validExistingDirectory(st, uint32(os.Getuid())) {
		return &Error{Code: "insecure_storage"}
	}
	return nil
}

func validExistingDirectory(st unix.Stat_t, uid uint32) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&07777 == 0700 && st.Uid == uid && st.Nlink != 0
}

func (s *existingLocal) directoryHandle(ctx context.Context) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.directory == nil {
		return nil, &Error{Code: "storage_closed"}
	}
	if err := checkExistingDirectory(s.directory); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(s.directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, existingOpenError(err)
	}
	f := os.NewFile(uintptr(fd), "existing-credential-transaction")
	if err := checkExistingDirectory(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func existingPrivateFile(directory *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, existingOpenError(err)
	}
	f := os.NewFile(uintptr(fd), "existing-private-credential")
	if err := checkExistingPrivateFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func checkExistingPrivateFile(f *os.File) error {
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || !validExistingPrivateFile(st, uint32(os.Getuid())) {
		return &Error{Code: "insecure_storage"}
	}
	return nil
}

func validExistingPrivateFile(st unix.Stat_t, uid uint32) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&07777 == 0600 && st.Uid == uid && st.Nlink == 1
}

func lockExisting(ctx context.Context, directory *os.File, key string) (*os.File, error) {
	// No O_CREAT: a missing old owner lock is not reconstructed or substituted.
	f, err := existingPrivateFile(directory, key+".lock", unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if err := checkExistingLock(directory, key, f); err != nil {
				unix.Flock(int(f.Fd()), unix.LOCK_UN)
				f.Close()
				return nil, err
			}
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, &Error{Code: "lock_failed"}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func checkExistingLock(directory *os.File, key string, file *os.File) error {
	var opened, named unix.Stat_t
	if unix.Fstat(int(file.Fd()), &opened) != nil || unix.Fstatat(int(directory.Fd()), key+".lock", &named, unix.AT_SYMLINK_NOFOLLOW) != nil || !validExistingPrivateFile(opened, uint32(os.Getuid())) || !validExistingPrivateFile(named, uint32(os.Getuid())) || opened.Dev != named.Dev || opened.Ino != named.Ino {
		return &Error{Code: "insecure_storage"}
	}
	return nil
}

func (s *existingLocal) withCredential(ctx context.Context, local *Local, ref Reference, fn func(Transaction) error) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	directory, err := s.directoryHandle(ctx)
	if err != nil {
		return err
	}
	defer directory.Close()
	lock, err := lockExisting(ctx, directory, local.key(ref))
	if err != nil {
		return err
	}
	defer lock.Close()
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkExistingDirectory(directory); err != nil {
		return err
	}
	file, err := existingPrivateFile(directory, local.key(ref)+".json", unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	tx := &existingTransaction{file: file, directory: directory, ctx: ctx, ref: ref}
	tx.active.Store(true)
	defer tx.active.Store(false)
	return fn(tx)
}

type existingTransaction struct {
	file      *os.File
	directory *os.File
	ctx       context.Context
	ref       Reference
	active    atomic.Bool
}

func (tx *existingTransaction) Read() (Record, error) {
	if !tx.active.Load() {
		return Record{}, &Error{Code: "storage_closed"}
	}
	if err := tx.ctx.Err(); err != nil {
		return Record{}, err
	}
	if err := checkExistingDirectory(tx.directory); err != nil {
		return Record{}, err
	}
	if err := checkExistingPrivateFile(tx.file); err != nil {
		return Record{}, err
	}
	return readPrivateRecord(io.NewSectionReader(tx.file, 0, 1<<20+1), tx.ref)
}

func (*existingTransaction) Write(Record) error { return &Error{Code: "read_only_storage"} }
func (*existingTransaction) Remove() error      { return &Error{Code: "read_only_storage"} }

func (s *existingLocal) list(ctx context.Context, local *Local) ([]Metadata, error) {
	directory, err := s.directoryHandle(ctx)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(4097)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, &Error{Code: "storage_unavailable"}
	}
	if len(entries) > 4096 {
		return nil, &Error{Code: "storage_limit"}
	}
	result := []Metadata{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
			return nil, &Error{Code: "invalid_record"}
		}
		metadata, err := existingMetadata(ctx, local, directory, key)
		if err != nil {
			return nil, err
		}
		result = append(result, metadata)
	}
	slices.SortFunc(result, func(a, b Metadata) int {
		if n := strings.Compare(a.Reference.Provider, b.Reference.Provider); n != 0 {
			return n
		}
		return strings.Compare(a.Reference.ID, b.Reference.ID)
	})
	return result, nil
}

func existingMetadata(ctx context.Context, local *Local, directory *os.File, key string) (Metadata, error) {
	lock, err := lockExisting(ctx, directory, key)
	if err != nil {
		return Metadata{}, err
	}
	defer lock.Close()
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if err := checkExistingDirectory(directory); err != nil {
		return Metadata{}, err
	}
	file, err := existingPrivateFile(directory, key+".json", unix.O_RDONLY)
	if err != nil {
		return Metadata{}, err
	}
	defer file.Close()
	var disk diskRecord
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &disk, json.RejectUnknownMembers(true)) != nil || disk.Metadata.Reference.Validate() != nil || local.key(disk.Metadata.Reference) != key {
		return Metadata{}, &Error{Code: "invalid_record"}
	}
	// Use the same strict record decoder as WithCredential under this existing
	// lock. List is still a per-entry metadata snapshot, not an account lease.
	record, err := readPrivateRecord(io.NewSectionReader(file, 0, 1<<20+1), disk.Metadata.Reference)
	if err != nil {
		return Metadata{}, err
	}
	return record.Metadata, nil
}
