//go:build !linux && !darwin

package mutation

import (
	"context"
	"errors"
	"os"
)

var errUnsupported = errors.New("safe mutation requires supported Linux or macOS local filesystem semantics")

func acquireLock(context.Context, string) (*os.File, error)    { return nil, errUnsupported }
func (s *Service) openParent(string) (*os.File, string, error) { return nil, "", errUnsupported }
func readTarget(*os.File, string) ([]byte, bool, os.FileMode, error) {
	return nil, false, 0, errUnsupported
}
func replaceTarget(*os.File, string, []byte, os.FileMode) (bool, error) { return false, errUnsupported }
