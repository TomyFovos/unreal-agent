//go:build linux || darwin

package credential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Local stores credentials in a private directory. It refuses unsafe permissions
// rather than silently pretending a shared/mounted filesystem is a secret store.
type Local struct{ directory string }

func OpenLocal(directory string) (*Local, error) {
	path, err := filepath.Abs(directory)
	if err != nil {
		return nil, &Error{Code: "storage_unavailable"}
	}
	if err = os.MkdirAll(path, 0700); err != nil {
		return nil, &Error{Code: "storage_unavailable"}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, &Error{Code: "insecure_storage"}
	}
	if err := owned(path, true); err != nil {
		return nil, err
	}
	return &Local{directory: path}, nil
}
func owned(path string, dir bool) error {
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil || st.Uid != uint32(os.Getuid()) {
		return &Error{Code: "insecure_storage"}
	}
	return nil
}
func (s *Local) key(r Reference) string {
	sum := sha256.Sum256([]byte(r.Provider + "\x00" + r.ID))
	return hex.EncodeToString(sum[:])
}
func (s *Local) checkDirectory() error {
	info, err := os.Lstat(s.directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return &Error{Code: "insecure_storage"}
	}
	return owned(s.directory, true)
}
func (s *Local) WithCredential(ctx context.Context, ref Reference, fn func(Transaction) error) error {
	if err := s.checkDirectory(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep lock inodes forever: deleting a flock file can split ownership.
	lockPath := filepath.Join(s.directory, s.key(ref)+".lock")
	f, err := openPrivate(lockPath, unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return &Error{Code: "lock_failed"}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(&localTransaction{store: s, ref: ref, path: filepath.Join(s.directory, s.key(ref)+".json")})
}
func openPrivate(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, &Error{Code: "not_found"}
		}
		return nil, &Error{Code: "storage_unavailable"}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	var st unix.Stat_t
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
		f.Close()
		return nil, &Error{Code: "insecure_storage"}
	}
	return f, nil
}

type diskRecord struct {
	Version      int      `json:"version"`
	Metadata     Metadata `json:"metadata"`
	Token        string   `json:"token"`
	RefreshToken string   `json:"refresh_token,omitzero"`
	AccountID    string   `json:"account_id,omitzero"`
}
type localTransaction struct {
	store *Local
	ref   Reference
	path  string
}

func (tx *localTransaction) Read() (Record, error) {
	file, err := openPrivate(tx.path, unix.O_RDONLY)
	if err != nil {
		return Record{}, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return Record{}, &Error{Code: "invalid_record"}
	}
	var d diskRecord
	if json.Unmarshal(raw, &d, json.RejectUnknownMembers(true)) != nil || d.Version != 1 || d.Metadata.Reference != tx.ref || d.Metadata.Revision == 0 {
		return Record{}, &Error{Code: "invalid_record"}
	}
	material := Material{Token: NewSecret(d.Token), RefreshToken: NewSecret(d.RefreshToken), AccountID: d.AccountID, Owner: d.Metadata.Owner, ExpiresAt: d.Metadata.ExpiresAt}
	if validateMaterial(tx.ref, material) != nil {
		return Record{}, &Error{Code: "invalid_record"}
	}
	return Record{Metadata: d.Metadata, Material: material}, nil
}
func (tx *localTransaction) Write(r Record) error {
	if r.Metadata.Reference != tx.ref || validateMaterial(tx.ref, r.Material) != nil {
		return &Error{Code: "invalid_record"}
	}
	revision := uint64(1)
	old, err := tx.Read()
	if err == nil {
		revision = old.Metadata.Revision + 1
	} else if !IsCode(err, "not_found") {
		return err
	}
	r.Metadata.Revision = revision
	r.Metadata.Owner = r.Material.Owner
	r.Metadata.ExpiresAt = r.Material.ExpiresAt
	d := diskRecord{Version: 1, Metadata: r.Metadata, Token: r.Material.Token.Reveal(), RefreshToken: r.Material.RefreshToken.Reveal(), AccountID: r.Material.AccountID}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 1<<20 {
		return &Error{Code: "invalid_record"}
	}
	temp, err := os.CreateTemp(tx.store.directory, ".credential-")
	if err != nil {
		return &Error{Code: "storage_unavailable"}
	}
	name := temp.Name()
	defer os.Remove(name)
	defer temp.Close()
	if _, err = temp.Write(raw); err != nil {
		return &Error{Code: "storage_unavailable"}
	}
	if err = temp.Sync(); err != nil {
		return &Error{Code: "storage_unavailable"}
	}
	if err = temp.Close(); err != nil {
		return &Error{Code: "storage_unavailable"}
	}
	if err = os.Rename(name, tx.path); err != nil {
		return &Error{Code: "storage_unavailable"}
	}
	return tx.syncDirectory()
}
func (tx *localTransaction) Remove() error {
	// Refuse symlink/hardlink/permission surprises before unlinking an existing record.
	f, err := openPrivate(tx.path, unix.O_RDONLY)
	if IsCode(err, "not_found") {
		return nil
	}
	if err != nil {
		return err
	}
	f.Close()
	if err = os.Remove(tx.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &Error{Code: "storage_unavailable"}
	}
	return tx.syncDirectory()
}
func (tx *localTransaction) syncDirectory() error {
	f, err := os.Open(tx.store.directory)
	if err != nil {
		return &Error{Code: "storage_unavailable"}
	}
	defer f.Close()
	if f.Sync() != nil {
		return &Error{Code: "storage_unavailable"}
	}
	return nil
}
func (s *Local) List(ctx context.Context) ([]Metadata, error) {
	if err := s.checkDirectory(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, &Error{Code: "storage_unavailable"}
	}
	result := []Metadata{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		f, err := openPrivate(filepath.Join(s.directory, entry.Name()), unix.O_RDONLY)
		if IsCode(err, "not_found") {
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
		f.Close()
		var d diskRecord
		if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &d, json.RejectUnknownMembers(true)) != nil || d.Version != 1 || d.Metadata.Reference.Validate() != nil || entry.Name() != s.key(d.Metadata.Reference)+".json" {
			return nil, &Error{Code: "invalid_record"}
		}
		// Atomic replacement makes each entry coherent; listing is a metadata snapshot,
		// not an account transaction or a guarantee against concurrent logout.
		result = append(result, d.Metadata)
	}
	slices.SortFunc(result, func(a, b Metadata) int {
		if n := strings.Compare(a.Reference.Provider, b.Reference.Provider); n != 0 {
			return n
		}
		return strings.Compare(a.Reference.ID, b.Reference.ID)
	})
	return result, nil
}
