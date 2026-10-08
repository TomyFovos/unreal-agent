package contextengine

import (
	"crypto/rand"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const DerivedDirectory = "context-v1"
const DerivedFile = "derived.json"

// Manifest contains no bodies, lexical terms, credentials or private replay
// items. It is advisory: canonical replay always rebuilds the in-memory index.
type Manifest struct {
	Version     int
	Checkpoint  Checkpoint
	Diagnostics *Diagnostics `json:",omitzero"`
}

type Cache struct {
	mu        sync.Mutex
	directory string
	status    string
	root      *os.Root
}

// OpenCache never makes a damaged/unsafe derived cache a Session failure.
// A private, pinned directory is required for every read and write.
func OpenCache(root, id string) *Cache {
	c := &Cache{status: "rebuilt-missing"}
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") {
		c.status = "unavailable"
		return c
	}
	base := filepath.Join(root, DerivedDirectory)
	for _, p := range []string{base, filepath.Join(base, id)} {
		if err := os.Mkdir(p, 0700); err != nil && !os.IsExist(err) {
			c.status = "unavailable"
			return c
		}
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			c.status = "unavailable"
			return c
		}
	}
	c.directory = filepath.Join(base, id)
	r, err := os.OpenRoot(c.directory)
	if err != nil {
		c.status = "unavailable"
		c.directory = ""
		return c
	}
	c.root = r
	info, err := r.Lstat(DerivedFile)
	if os.IsNotExist(err) {
		return c
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		c.status = "unavailable"
		c.directory = ""
		return c
	}
	f, err := r.Open(DerivedFile)
	if err != nil {
		c.status = "unavailable"
		return c
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		c.status = "unavailable"
		return c
	}
	data, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	var m Manifest
	if err != nil || len(data) > 8<<20 || json.Unmarshal(data, &m, json.RejectUnknownMembers(true)) != nil {
		c.status = "rebuilt-corrupt"
		return c
	}
	if m.Version != Version || m.Checkpoint.Version != 0 && m.Checkpoint.Version != Version {
		c.status = "rebuilt-version"
		return c
	}
	// Never trust this file for selection, even if it is well formed.
	c.status = "rebuilt-canonical"
	return c
}

func (c *Cache) Status() string { return c.status }
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.root != nil {
		r := c.root
		c.root = nil
		return r.Close()
	}
	return nil
}

func (c *Cache) Write(m Manifest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.directory == "" || c.root == nil {
		return &Error{"derived_cache_unavailable"}
	}
	r := c.root
	info, err := r.Stat(".")
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return &Error{"derived_cache_unavailable"}
	}
	// The random temporary name and atomic rename affect only derived state.
	name := ".derived-" + rand.Text()
	f, err := r.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return &Error{"derived_cache_unavailable"}
	}
	defer r.Remove(name)
	data, err := json.Marshal(m)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = r.Rename(name, DerivedFile)
	}
	if err == nil {
		d, e := r.Open(".")
		if e == nil {
			err = d.Sync()
			d.Close()
		} else {
			err = e
		}
	}
	if err != nil {
		return &Error{"derived_cache_unavailable"}
	}
	return nil
}

func (e *Engine) Manifest() Manifest {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := Manifest{Version: Version, Checkpoint: cloneCheckpoint(e.checkpoint)}
	if e.last != nil {
		d := *e.last
		m.Diagnostics = &d
	}
	return m
}
func (e *Engine) SetCacheStatus(status string) { e.mu.Lock(); defer e.mu.Unlock(); e.cache = status }
