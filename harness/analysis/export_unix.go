//go:build linux || darwin

package analysis

import (
	"context"
	"github.com/unreallabsai/unreal-agent/internal/privateexport"
	"strings"
)

func Export(ctx context.Context, dir, format string, r Report) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := Encode(r, format)
	if err != nil {
		return "", err
	}
	ext := "json"
	if strings.EqualFold(format, "markdown") {
		ext = "md"
	}
	return privateexport.Write(ctx, dir, "analysis", ext, data)
}
