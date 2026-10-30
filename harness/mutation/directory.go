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

func (s *Service) ReadDir(ctx context.Context, path string) ([]Entry, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.root, path)
	}
	path = filepath.Clean(path)
	if path == s.alias {
		path = s.root
	}
	if path != s.root {
		var err error
		path, err = s.path(path, false)
		if err != nil {
			return nil, err
		}
	}
	if err := s.authorize(ctx, path, false); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A synthetic basename pins the requested directory with the same nofollow
	// traversal used by file operations. No synthetic file is opened or created.
	parent, _, err := s.authorizedParent(ctx, filepath.Join(path, ".directory-read"), false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	entries, err := parent.ReadDir(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	output := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), ".unreal-mutation-") {
			continue
		}
		if _, err := s.path(filepath.Join(path, entry.Name()), false); err != nil {
			continue
		}
		if entry.IsDir() || entry.Type().IsRegular() {
			output = append(output, Entry{Name: entry.Name(), Directory: entry.IsDir()})
		}
	}
	slices.SortFunc(output, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return output, nil
}
