//go:build !linux && !darwin

package tui

import (
	"context"
	"errors"
)

func Terminal(context.Context, Config) error {
	return errors.New("tui: terminal requires Linux/macOS; use WSL on Windows")
}
