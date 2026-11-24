package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"golang.org/x/sys/unix"
)

// subagentModelScope owns a persistent choice for each top-level Session.
// A lead uses its parent's scope for worker starts.
type subagentModelScope struct {
	defaultFile      string
	sessionDirectory string
	parent           session.ID
}

var errReadSubagentModels = errors.New("read subagent models")

func (s subagentModelScope) directory() string {
	return s.defaultFile + ".sessions"
}

func (s subagentModelScope) file(id session.ID) string {
	key := sha256.Sum256([]byte(absolutePath(s.sessionDirectory) + "\x00" + string(id)))
	return filepath.Join(s.directory(), fmt.Sprintf("%x.json", key))
}

func (s subagentModelScope) read(ctx context.Context, id session.ID) (subagentModel, error) {
	if s.defaultFile == "" {
		return subagentModel{}, nil
	}
	if s.parent != "" {
		id = s.parent
	} else if id == "" {
		if owner, ok := host.SessionFromContext(ctx); ok {
			id = owner.ID
		}
	}
	if id == "" {
		return readSubagentModel(s.defaultFile)
	}
	path := s.file(id)
	model, err := readSubagentModel(path)
	if err == nil && model.Version == 0 {
		err = s.locked(func() error {
			var err error
			model, err = readSubagentModel(path)
			if err != nil || model.Version != 0 {
				return err
			}
			model, err = readSubagentModel(s.defaultFile)
			if err != nil {
				return err
			}
			return writeSubagentModel(path, snapshotSubagentModel(model))
		})
	}
	if model.Provider == "" && model.Roles != nil && *model.Roles == (subagentRoleModels{}) {
		model = subagentModel{}
	}
	return model, err
}

func snapshotSubagentModel(model subagentModel) subagentModel {
	if model.Version == 0 {
		// Persist an empty selection so a later default also leaves these
		// Sessions on their original template defaults.
		return subagentModel{Version: 2, Roles: &subagentRoleModels{}}
	}
	return model
}

// update validates every affected scope before writing. Each snapshot is
// published atomically and the lock serializes changes across Hosts.
func (s subagentModelScope) update(id session.ID, all bool, change func(*subagentModel) bool) (subagentModel, error) {
	caller := s.file(id)
	var result subagentModel
	err := s.locked(func() error {
		files := []string{caller}
		if all {
			entries, err := os.ReadDir(s.directory())
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if entry.Type().IsRegular() && len(entry.Name()) == sha256.Size*2+len(".json") && strings.HasSuffix(entry.Name(), ".json") {
					if path := filepath.Join(s.directory(), entry.Name()); path != caller {
						files = append(files, path)
					}
				}
			}
			files = append(files, s.defaultFile)
		}
		updates := make([]subagentModel, len(files))
		for i, path := range files {
			model, err := readSubagentModel(path)
			if err == nil && model.Version == 0 && path != s.defaultFile {
				model, err = readSubagentModel(s.defaultFile)
			}
			if err != nil {
				return fmt.Errorf("%w: %v", errReadSubagentModels, err)
			}
			if !change(&model) {
				return nil
			}
			if path != s.defaultFile {
				model = snapshotSubagentModel(model)
			}
			updates[i] = model
		}
		// Prepare every file before publishing any changes so a failed
		// write leaves all affected Sessions on their previous choices.
		prepared := make([]string, len(files))
		defer func() {
			for _, path := range prepared {
				if path != "" {
					_ = os.Remove(path)
				}
			}
		}()
		for i, path := range files {
			if updates[i].Version != 0 {
				temporary, err := prepareSubagentModel(path, updates[i])
				if err != nil {
					return err
				}
				prepared[i] = temporary
			}
		}
		for i, path := range prepared {
			if path != "" {
				if err := os.Rename(path, files[i]); err != nil {
					return err
				}
				prepared[i] = ""
			}
		}
		result = updates[0]
		return nil
	})
	return result, err
}

func (s subagentModelScope) locked(action func() error) error {
	if err := os.MkdirAll(s.directory(), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(s.directory(), ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return err
	}
	return action()
}
