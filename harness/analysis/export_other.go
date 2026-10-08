//go:build !linux && !darwin

package analysis

import (
	"context"
	"errors"
)

func Export(context.Context, string, string, Report) (string, error) {
	return "", errors.New("private exports require Linux or macOS")
}
