package viewer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

// The view a person last chose in a Session's terminal, kept beside its
// record so that reopening the Session shows the same view.
const viewPreferenceSuffix = ".view"

var viewPreferences = map[string]bool{"chat": true, "orchestration": true, "split": true}

func viewPreferencePath(directory string, id session.ID) (string, error) {
	name := string(id)
	if name == "" || directory == "" {
		return "", fs.ErrNotExist
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return "", fs.ErrNotExist
		}
	}
	return filepath.Join(directory, name+viewPreferenceSuffix), nil
}

func readViewPreference(directory string, id session.ID) (string, error) {
	path, err := viewPreferencePath(directory, id)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	view := strings.TrimSpace(string(data))
	if !viewPreferences[view] {
		return "", fs.ErrNotExist
	}
	return view, nil
}

func writeViewPreference(directory string, id session.ID, view string) error {
	if !viewPreferences[view] {
		return errors.New("viewer: unknown view")
	}
	path, err := viewPreferencePath(directory, id)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".view-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err = temporary.WriteString(view + "\n"); err == nil {
		err = temporary.Chmod(0o600)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// SavedView returns the view last chosen for the parent Session, or "" when
// none was saved or the Host does not keep one.
func (r Remote) SavedView(ctx context.Context) string {
	reply, err := r.exchange(ctx, extensionRequest{Action: "view.get", ParentID: r.ParentID})
	if err != nil || !viewPreferences[reply.Display] {
		return ""
	}
	return reply.Display
}

// SaveView records the view the person chose for the parent Session.
func (r Remote) SaveView(ctx context.Context, view string) error {
	_, err := r.exchange(ctx, extensionRequest{Action: "view.set", ParentID: r.ParentID, Display: view})
	return err
}
