package native

import (
	"bytes"
	"context"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

const maxFileOutputBytes = 65_536

type ReadFile struct {
	Path   string      `json:"path"`
	Ranges []LineRange `json:"ranges,omitempty"`
}

type LineRange struct {
	StartLine int `json:"start_line,omitempty"`
	Lines     int `json:"lines,omitempty"`
}

type ReadFileResult struct {
	Path     string             `json:"path"`
	Code     string             `json:"code"`
	Reason   mutation.Reason    `json:"reason,omitempty"`
	Revision *mutation.Revision `json:"revision,omitempty"`
	Ranges   []ReadRangeResult  `json:"ranges,omitempty"`
}

type ReadRangeResult struct {
	StartLine int    `json:"start_line"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (e Executor) readFile(ctx context.Context, path string) (Result, string) {
	result := Result{Version: Version, Path: path, Code: "ok"}
	snap, err := e.Files.Snapshot(ctx, path)
	if err != nil {
		result.Code = errorCode(ctx, err)
		result.Reason = mutation.FailureReason(err)
		result.Denial = permission.Failure(err)
		return result, ""
	}
	result.Revision = &snap.Revision
	if !snap.Revision.Exists {
		result.Code = "not_found"
		return result, ""
	}
	if !utf8.Valid(snap.Data) || bytes.IndexByte(snap.Data, 0) >= 0 {
		result.Code, result.Binary = "binary", true
		return result, ""
	}
	return result, string(snap.Data)
}

func (e Executor) readBatch(ctx context.Context, r Request) Result {
	out := Result{Version: Version, Code: "ok"}
	remaining := min(r.MaxBytes, maxFileOutputBytes)
	anyRead := false
	for _, file := range r.Files {
		if ctx.Err() != nil {
			out.Code = "canceled"
			return out
		}
		// Account for paths as well as content, as search does for matches.
		if remaining < len(file.Path) {
			out.Truncated = true
			break
		}
		remaining -= len(file.Path)
		result, text := e.readFile(ctx, file.Path)
		if result.Code == "canceled" {
			out.Code = "canceled"
			return out
		}
		entry := ReadFileResult{Path: file.Path, Code: result.Code, Reason: result.Reason, Revision: result.Revision}
		if result.Code == "ok" {
			lines := strings.SplitAfter(text, "\n")
			ranges := file.Ranges
			if len(ranges) == 0 {
				ranges = []LineRange{{}}
			}
			for _, requested := range ranges {
				// Empty excerpts still occupy space in the result.
				if remaining == 0 {
					out.Truncated = true
					break
				}
				remaining--
				part := readRange(lines, requested, remaining)
				entry.Ranges = append(entry.Ranges, part)
				remaining -= len(part.Content)
				if part.Truncated {
					out.Truncated = true
					break
				}
			}
		}
		anyRead = anyRead || result.Code == "ok" || result.Code == "binary"
		out.Files = append(out.Files, entry)
		if out.Truncated {
			break
		}
	}
	if !out.Truncated && !anyRead {
		out.Code = "read_failed"
	}
	return out
}

func readRange(lines []string, requested LineRange, limit int) ReadRangeResult {
	start := max(1, requested.StartLine)
	result := ReadRangeResult{StartLine: start}
	if start > len(lines) {
		return result
	}
	selected := lines[start-1:]
	if requested.Lines > 0 {
		selected = selected[:min(len(selected), requested.Lines)]
	}
	var content strings.Builder
	for _, line := range selected {
		text, truncated := BoundText(line, limit-content.Len())
		content.WriteString(text)
		if truncated {
			result.Truncated = true
			break
		}
	}
	result.Content = content.String()
	return result
}
