//go:build !linux && !darwin

package testclaude

import (
	"errors"
	"os"
)

func openSyntheticProgress(string) (*os.File, error) {
	return nil, errors.New("synthetic progress unavailable on this platform")
}
