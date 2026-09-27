package mutation

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Entry struct {
	Name      string
	Directory bool
}

func (s *Service) ReadDir(ctx context.Context, path string, limit int) ([]Entry, bool, error) {
	if limit < 1 || limit > 20000 {
		return nil, false, errors.New("invalid directory entry limit")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.root, path)
	}
	path = filepath.Clean(path)
	if path != s.root {
		var err error
		path, err = s.Path(path)
		if err != nil {
			return nil, false, err
		}
	}
	if err := s.authorize(ctx, path, false); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// A synthetic basename pins the requested directory with the same nofollow
	// traversal used by file operations. No synthetic file is opened or created.
	parent, _, err := s.openParent(filepath.Join(path, ".directory-read"))
	if err != nil {
		return nil, false, err
	}
	defer parent.Close()
	entries, err := parent.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	// Refuse an oversized directory instead of returning an unstable filesystem-order
	// subset as if it were a sorted complete search.
	if len(entries) > limit {
		return nil, true, nil
	}
	output := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), ".unreal-mutation-") {
			continue
		}
		if entry.IsDir() || entry.Type().IsRegular() {
			output = append(output, Entry{Name: entry.Name(), Directory: entry.IsDir()})
		}
	}
	slices.SortFunc(output, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return output, false, nil
}
