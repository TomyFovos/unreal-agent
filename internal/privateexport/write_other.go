//go:build !linux && !darwin

package privateexport

import (
	"context"
	"errors"
	"io"
)

func Write(context.Context, string, string, string, []byte) (string, error) {
	return "", errors.New("private exports require Linux or macOS")
}

func WriteStream(context.Context, string, string, string, func(io.Writer) error) (string, error) {
	return "", errors.New("private exports require Linux or macOS")
}

func CreateRuntimeConfig(context.Context, string, []byte) error {
	return errors.New("private runtime configurations require Linux or macOS")
}
