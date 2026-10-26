//go:build linux

package credential

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func existingFixture(t *testing.T) (*Local, Reference, Material) {
	t.Helper()
	local := testLocal(t)
	ref := Reference{Provider: "synthetic-continuation", ID: "original-owner", Method: OAuth}
	material := Material{Token: NewSecret("SYNTHETIC_ONLY_ACCESS_CANARY"), RefreshToken: NewSecret("SYNTHETIC_ONLY_REFRESH_CANARY"), AccountID: "synthetic-account", Owner: Managed, ExpiresAt: time.Now().Add(time.Hour)}
	if err := NewManager(local, nil).Login(t.Context(), ref, material); err != nil {
		t.Fatal(err)
	}
	return local, ref, material
}

func existingRead(t *testing.T, store *Local, ref Reference) (Record, error) {
	t.Helper()
	var record Record
	err := store.WithCredential(t.Context(), ref, func(tx Transaction) error {
		var err error
		record, err = tx.Read()
		return err
	})
	return record, err
}

func TestExistingLocalReadOnlyRoundtripKeepsOriginalRecord(t *testing.T) {
	local, ref, material := existingFixture(t)
	path := filepath.Join(local.directory, local.key(ref)+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var retained Transaction
	if err := store.WithCredential(t.Context(), ref, func(tx Transaction) error {
		retained = tx
		for range 2 {
			record, err := tx.Read()
			if err != nil || record.Metadata.Reference != ref || record.Metadata.Revision != 1 || record.Material.Token.Reveal() != material.Token.Reveal() || record.Material.AccountID != material.AccountID {
				return errors.New("synthetic existing credential roundtrip mismatch")
			}
			if !IsCode(tx.Write(record), "read_only_storage") || !IsCode(tx.Remove(), "read_only_storage") {
				return errors.New("existing store allowed a credential mutation")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.Read(); !IsCode(err, "storage_closed") {
		t.Fatal("transaction escaped its lifetime", err)
	}
	for range 2 {
		metadata, err := store.List(t.Context())
		if err != nil || len(metadata) != 1 || metadata[0].Reference != ref {
			t.Fatal("existing metadata list lost its independently opened directory offset", err)
		}
		raw, _ := json.Marshal(metadata)
		if bytes.Contains(raw, []byte(material.Token.Reveal())) || bytes.Contains(raw, []byte(material.RefreshToken.Reveal())) {
			t.Fatal("metadata list contains synthetic credential material")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil || !bytes.Equal(before, after) || !os.SameFile(beforeInfo, afterInfo) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("read-only continuation changed credential bytes, inode or mtime", err)
	}
}

func TestExistingLocalNeverCreatesOrRepairsStorage(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-created")
	if _, err := OpenExistingLocal(missing); !IsCode(err, "not_found") {
		t.Fatal("missing directory did not fail closed", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("existing opener created a directory", err)
	}
	local, ref, _ := existingFixture(t)
	if err := os.Chmod(local.directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExistingLocal(local.directory); !IsCode(err, "insecure_storage") {
		t.Fatal("unsafe directory accepted", err)
	}
	if info, err := os.Stat(local.directory); err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("existing opener silently repaired source permissions", err)
	}
	if err := os.Chmod(local.directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(local.directory, local.key(ref)+".lock")); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := existingRead(t, store, ref); !IsCode(err, "not_found") {
		t.Fatal("missing original lock did not fail closed", err)
	}
	if _, err := os.Lstat(filepath.Join(local.directory, local.key(ref)+".lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("existing transaction recreated its missing owner lock", err)
	}
}

func TestExistingLocalRejectsSymlinkDirectoryComponents(t *testing.T) {
	local, _, _ := existingFixture(t)
	link := filepath.Join(t.TempDir(), "directory-link")
	if err := os.Symlink(local.directory, link); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(t.TempDir(), "ancestor-link")
	if err := os.Symlink(filepath.Dir(local.directory), ancestorLink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(ancestorLink, filepath.Base(local.directory)), local.directory + "/../credentials", ""} {
		if _, err := OpenExistingLocal(path); !IsCode(err, "insecure_storage") {
			t.Fatal("unsafe source directory path accepted", err)
		}
	}
}

func TestExistingLocalDirectoryRenameCannotRedirectSource(t *testing.T) {
	local, ref, original := existingFixture(t)
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldParent := filepath.Dir(local.directory)
	movedParent := oldParent + "-moved"
	if err := os.Rename(oldParent, movedParent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(movedParent) })
	replacement, err := OpenLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	other := original
	other.Token = NewSecret("SYNTHETIC_ONLY_REPLACEMENT_CANARY")
	if err := NewManager(replacement, nil).Login(t.Context(), ref, other); err != nil {
		t.Fatal(err)
	}
	record, err := existingRead(t, store, ref)
	if err != nil || record.Material.Token.Reveal() != original.Token.Reveal() {
		t.Fatal("ancestor replacement redirected the pinned existing directory", err)
	}
}

func TestExistingLocalRecordDescriptorSurvivesLeafReplacement(t *testing.T) {
	local, ref, original := existingFixture(t)
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	path := filepath.Join(local.directory, local.key(ref)+".json")
	if err := store.WithCredential(t.Context(), ref, func(tx Transaction) error {
		// The callback starts after the original lock and record are open.
		if err := os.Rename(path, path+".preserved"); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(`{"synthetic_replacement":true}`), 0600); err != nil {
			return err
		}
		record, err := tx.Read()
		if err != nil || record.Material.Token.Reveal() != original.Token.Reveal() {
			return errors.New("pinned record was redirected to a replacement leaf")
		}
		flags, err := unix.FcntlInt(tx.(*existingTransaction).file.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			return errors.New("credential descriptor is inheritable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExistingLocalRejectsUnsafeRecordAndLock(t *testing.T) {
	for _, leaf := range []string{"record", "lock"} {
		for _, attack := range []string{"symlink", "hardlink", "mode", "fifo"} {
			t.Run(leaf+"_"+attack, func(t *testing.T) {
				local, ref, _ := existingFixture(t)
				store, err := OpenExistingLocal(local.directory)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				suffix := ".json"
				if leaf == "lock" {
					suffix = ".lock"
				}
				path := filepath.Join(local.directory, local.key(ref)+suffix)
				switch attack {
				case "symlink":
					if err := os.Rename(path, path+".preserved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(path+".preserved", path); err != nil {
						t.Fatal(err)
					}
				case "hardlink":
					if err := os.Link(path, filepath.Join(t.TempDir(), "synthetic-hardlink")); err != nil {
						t.Fatal(err)
					}
				case "mode":
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := unix.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
				called := false
				err = store.WithCredential(t.Context(), ref, func(Transaction) error { called = true; return nil })
				if !IsCode(err, "insecure_storage") || called {
					t.Fatal("unsafe existing credential object reached callback", err)
				}
			})
		}
	}
}

func TestExistingLocalSharesStrictRecordDecoder(t *testing.T) {
	for _, kind := range []string{"corrupt", "version", "reference", "revision", "owner", "empty_token", "expiry"} {
		t.Run(kind, func(t *testing.T) {
			local, ref, _ := existingFixture(t)
			path := filepath.Join(local.directory, local.key(ref)+".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk diskRecord
			if err := json.Unmarshal(raw, &disk); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "corrupt":
				raw = []byte("{synthetic unfinished")
			case "version":
				disk.Version = 2
			case "reference":
				disk.Metadata.Reference.ID = "different-synthetic-owner"
			case "revision":
				disk.Metadata.Revision = 0
			case "owner":
				disk.Metadata.Owner = "unknown-synthetic-owner"
			case "empty_token":
				disk.Token = ""
			case "expiry":
				disk.Metadata.ExpiresAt = time.Time{}
			}
			if kind != "corrupt" {
				raw, err = json.Marshal(disk)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			store, err := OpenExistingLocal(local.directory)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if _, err := existingRead(t, store, ref); !IsCode(err, "invalid_record") {
				t.Fatal("existing reader loosened record decoding", err)
			}
		})
	}
}

func TestExistingLocalCannotRefreshExpiredCredential(t *testing.T) {
	local, ref, material := existingFixture(t)
	material.ExpiresAt = time.Now().Add(-time.Hour)
	if err := NewManager(local, nil).Login(t.Context(), ref, material); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var calls atomic.Int64
	manager := NewManager(store, map[string]RefreshFunc{ref.Provider: func(context.Context, Reference, Material) (Material, error) {
		calls.Add(1)
		return material, nil
	}})
	if _, err := manager.Resolve(t.Context(), ref); !IsCode(err, "refresh_persist_failed") || calls.Load() != 0 {
		t.Fatal("read-only continuation entered a refresh exchange", err)
	}
}

func TestExistingLocalLockCancellationAndClose(t *testing.T) {
	local, ref, _ := existingFixture(t)
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := openPrivate(filepath.Join(local.directory, local.key(ref)+".lock"), unix.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	called := false
	if err := store.WithCredential(ctx, ref, func(Transaction) error { called = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatal("blocked old-owner lock did not honor cancellation", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal("Close is not idempotent", err)
	}
	if _, err := existingRead(t, store, ref); !IsCode(err, "storage_closed") {
		t.Fatal("closed directory accepted a new transaction", err)
	}
	if _, err := store.List(t.Context()); !IsCode(err, "storage_closed") {
		t.Fatal("closed directory accepted List", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal("ordinary Local Close should be a no-op", err)
	}
	if local.existing != nil {
		t.Fatal("normal OpenLocal acquired continuation resources")
	}
}

func TestExistingLocalStatChecksRejectSyntheticForeignOwners(t *testing.T) {
	uid := uint32(os.Getuid())
	directory := unix.Stat_t{Mode: unix.S_IFDIR | 0700, Uid: uid, Nlink: 1}
	file := unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: uid, Nlink: 1}
	if !validExistingDirectory(directory, uid) || !validExistingPrivateFile(file, uid) {
		t.Fatal("normal owned private stat rejected")
	}
	// An unprivileged test cannot chown fixtures to another UID. These inputs
	// test the exact stat predicate called after real Fstat; no sudo is used.
	directory.Uid++
	file.Uid++
	if validExistingDirectory(directory, uid) || validExistingPrivateFile(file, uid) {
		t.Fatal("foreign UID accepted by live descriptor stat predicate")
	}
}

func TestExistingLocalChangedDirectoryModeFailsBeforeRead(t *testing.T) {
	local, ref, _ := existingFixture(t)
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Chmod(local.directory, 0755); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := store.WithCredential(t.Context(), ref, func(Transaction) error { called = true; return nil }); !IsCode(err, "insecure_storage") || called {
		t.Fatal("existing private directory permission change was ignored", err)
	}
}

func TestExistingLocalFailureDoesNotRevealSyntheticMaterial(t *testing.T) {
	local, ref, material := existingFixture(t)
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.WithCredential(t.Context(), ref, func(tx Transaction) error {
		record, err := tx.Read()
		if err != nil {
			return err
		}
		return tx.Write(record)
	})
	if err == nil {
		t.Fatal("synthetic read-only error was not produced")
	}
	printed := err.Error()
	if strings.Contains(printed, material.Token.Reveal()) || strings.Contains(printed, material.RefreshToken.Reveal()) {
		t.Fatal("typed existing-store error exposed credential material")
	}
}

func TestExistingLocalLockIdentityCannotBeSubstituted(t *testing.T) {
	local, ref, _ := existingFixture(t)
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	directory, err := store.existing.directoryHandle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	key := local.key(ref)
	lock, err := existingPrivateFile(directory, key+".lock", unix.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	path := filepath.Join(local.directory, key+".lock")
	if err := os.Rename(path, path+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkExistingLock(directory, key, lock); !IsCode(err, "insecure_storage") {
		t.Fatal("opened lock and named lock diverged without failure", err)
	}
}

func TestExistingLocalPreservesRefreshPendingEvidence(t *testing.T) {
	local, ref, _ := existingFixture(t)
	if err := local.WithCredential(t.Context(), ref, func(tx Transaction) error {
		record, err := tx.Read()
		if err != nil {
			return err
		}
		record.Metadata.RefreshPending = true
		return tx.Write(record)
	}); err != nil {
		t.Fatal(err)
	}
	store, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, err := existingRead(t, store, ref)
	if err != nil || !record.Metadata.RefreshPending || record.Metadata.Revision != 2 {
		t.Fatal("existing reader erased refresh uncertainty", err)
	}
	if _, err := NewManager(store, nil).Resolve(t.Context(), ref); !IsCode(err, "reauth_required") {
		t.Fatal("refresh-pending credential was published as usable", err)
	}
}

func TestExistingLocalAtUsesOriginalOwnerAfterPathReplacement(t *testing.T) {
	local, ref, original := existingFixture(t)
	parent := filepath.Dir(local.directory)
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	moved := parent + "-original-owner"
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(moved) })
	replacement, err := OpenLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	other := original
	other.Token = NewSecret("SYNTHETIC_ONLY_OTHER_OWNER_PATH")
	if err := NewManager(replacement, nil).Login(t.Context(), ref, other); err != nil {
		t.Fatal(err)
	}
	// Prove the fixture actually redirected the old pathname; a normal reopen
	// now observes the replacement. The At API must stay with source's inode.
	pathStore, err := OpenExistingLocal(local.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer pathStore.Close()
	if r, err := existingRead(t, pathStore, ref); err != nil || r.Material.Token.Reveal() != other.Token.Reveal() {
		t.Fatal("fixture did not replace the pathname with another synthetic credential", err)
	}
	store, err := OpenExistingLocalAt(source, "credentials")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if r, err := existingRead(t, store, ref); err != nil || r.Material.Token.Reveal() != original.Token.Reveal() {
		t.Fatal("credential source escaped its pinned owner root", err)
	}
}

func TestExistingLocalAtRejectsUnsafeChildAndSource(t *testing.T) {
	local, _, _ := existingFixture(t)
	if err := os.Chmod(filepath.Dir(local.directory), 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(filepath.Dir(local.directory))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, child := range []string{"", ".", "..", "../credentials", "/credentials", "credentials/subdir", "credentials\\subdir"} {
		if store, err := OpenExistingLocalAt(source, child); !IsCode(err, "insecure_storage") || store != nil {
			t.Fatal("non-basename credential lookup was accepted", err)
		}
	}
	if store, err := OpenExistingLocalAt(nil, "credentials"); !IsCode(err, "insecure_storage") || store != nil {
		t.Fatal("nil source root accepted", err)
	}
	if err := os.Symlink(local.directory, filepath.Join(filepath.Dir(local.directory), "credential-link")); err != nil {
		t.Fatal(err)
	}
	if store, err := OpenExistingLocalAt(source, "credential-link"); !IsCode(err, "insecure_storage") || store != nil {
		t.Fatal("source-relative symlink accepted", err)
	}
	if err := os.Chmod(filepath.Dir(local.directory), 0755); err != nil {
		t.Fatal(err)
	}
	if store, err := OpenExistingLocalAt(source, "credentials"); !IsCode(err, "insecure_storage") || store != nil {
		t.Fatal("non-private pinned source root accepted", err)
	}
}
