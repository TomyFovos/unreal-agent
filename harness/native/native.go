// Package native executes the bounded file tools. Translators only encode plans.
package native

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

const Version = 1
const MaxResults = 1000
const MaxScanEntries = 20000
const MaxOutputBytes = 64 << 10

type Request struct {
	Version    int                `json:"version"`
	Action     string             `json:"action"`
	Path       string             `json:"path"`
	Expected   *mutation.Revision `json:"expected,omitempty"`
	Content    string             `json:"content,omitempty"`
	OldText    string             `json:"old_text,omitempty"`
	NewText    string             `json:"new_text,omitempty"`
	ReplaceAll bool               `json:"replace_all,omitempty"`
	Pattern    string             `json:"pattern,omitempty"`
	Glob       string             `json:"glob,omitempty"`
	StartLine  int                `json:"start_line,omitempty"`
	Lines      int                `json:"lines,omitempty"`
	Limit      int                `json:"limit,omitempty"`
	MaxBytes   int                `json:"max_bytes,omitempty"`
}

func (r Request) Validate() error {
	if r.Version != Version {
		return errors.New("unsupported native plan version")
	}
	if r.Path == "" || len(r.Path) > 4096 || strings.IndexByte(r.Path, 0) >= 0 {
		return errors.New("path is required")
	}
	if r.MaxBytes < 0 || r.MaxBytes > MaxOutputBytes || r.Limit < 0 || r.Limit > MaxResults || r.StartLine < 0 || r.Lines < 0 || r.Lines > 10000 {
		return errors.New("range/output limit out of bounds")
	}
	switch r.Action {
	case "read":
	case "write", "edit":
		if r.Expected == nil {
			return errors.New("expected revision is required")
		}
		if err := r.Expected.Validate(); err != nil {
			return err
		}
		if len(r.Content)+len(r.OldText)+len(r.NewText) > mutation.MaxFileBytes {
			return errors.New("payload exceeds limit")
		}
		if r.Action == "edit" && r.OldText == "" {
			return errors.New("old_text must be nonempty")
		}
	case "grep":
		if len(r.Pattern) > 8192 {
			return errors.New("pattern exceeds limit")
		}
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return errors.New("invalid regular expression")
		}
	case "glob":
	default:
		return errors.New("unknown native action")
	}
	if r.Glob != "" {
		if _, err := MatchGlob(r.Glob, ""); err != nil {
			return err
		}
	}
	return nil
}

type Match struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}
type Result struct {
	Denial    *permission.Error  `json:"denial,omitempty"`
	Version   int                `json:"version"`
	Code      string             `json:"code"`
	Path      string             `json:"path,omitempty"`
	Revision  *mutation.Revision `json:"revision,omitempty"`
	Content   string             `json:"content,omitempty"`
	StartLine int                `json:"start_line,omitempty"`
	Binary    bool               `json:"binary,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
	Matches   []Match            `json:"matches,omitempty"`
	Skipped   int                `json:"skipped,omitempty"`
	Mutation  *mutation.Result   `json:"mutation,omitempty"`
}
type Executor struct{ Files *mutation.Service }

func (e Executor) Execute(ctx context.Context, id string, r Request) Result {
	result := Result{Version: Version, Code: "invalid"}
	if r.Validate() != nil || e.Files == nil {
		return result
	}
	if ctx.Err() != nil {
		result.Code = "canceled"
		return result
	}
	if r.MaxBytes == 0 {
		r.MaxBytes = 16 << 10
	}
	if r.Limit == 0 {
		r.Limit = 200
	}
	if r.Action == "grep" || r.Action == "glob" {
		return e.search(ctx, r)
	}
	result.Path = r.Path
	if r.Action == "edit" {
		return e.edit(ctx, id, r)
	}
	if r.Action == "write" {
		m := e.Files.Execute(ctx, id, mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: r.Path, Expected: *r.Expected, Content: []byte(r.Content)}}})
		result.Code = string(m.Code)
		result.Mutation = &m
		result.Denial = m.Denial
		return result
	}
	snap, err := e.Files.Snapshot(ctx, r.Path)
	if err != nil {
		result.Code = errorCode(ctx, err)
		result.Denial = permission.Failure(err)
		return result
	}
	result.Revision = &snap.Revision
	if !snap.Revision.Exists {
		result.Code = "not_found"
		return result
	}
	if !utf8.Valid(snap.Data) || strings.IndexByte(string(snap.Data), 0) >= 0 {
		result.Code = "binary"
		result.Binary = true
		return result
	}
	lines := strings.SplitAfter(string(snap.Data), "\n")
	start := r.StartLine
	if start == 0 {
		start = 1
	}
	result.StartLine = start
	if start > len(lines) {
		result.Code = "ok"
		return result
	}
	end := len(lines)
	if r.Lines > 0 && start-1+r.Lines < end {
		end = start - 1 + r.Lines
	}
	content := strings.Join(lines[start-1:end], "")
	result.Content, result.Truncated = BoundText(content, r.MaxBytes)
	result.Code = "ok"
	return result
}
func errorCode(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "canceled"
	}
	type denial interface{ PermissionDenied() bool }
	var denied denial
	if permission.Failure(err) != nil || errors.As(err, &denied) {
		return "permission_denied"
	}
	return "read_failed"
}
func BoundText(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text, true
}
func MatchGlob(pattern, path string) (bool, error) {
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)
	if len(pattern) > 4096 {
		return false, errors.New("glob exceeds limit")
	}
	parts := strings.Split(pattern, "/")
	names := strings.Split(path, "/")
	for _, part := range parts {
		if part != "**" {
			if _, err := filepath.Match(part, ""); err != nil {
				return false, errors.New("invalid glob")
			}
		}
	}
	memo := map[[2]int]bool{}
	known := map[[2]int]bool{}
	var match func(int, int) bool
	match = func(p, n int) (result bool) {
		key := [2]int{p, n}
		if known[key] {
			return memo[key]
		}
		defer func() { known[key] = true; memo[key] = result }()
		if p == len(parts) {
			return n == len(names)
		}
		if parts[p] == "**" {
			return match(p+1, n) || (n < len(names) && match(p, n+1))
		}
		if n == len(names) {
			return false
		}
		ok, _ := filepath.Match(parts[p], names[n])
		return ok && match(p+1, n+1)
	}
	return match(0, 0), nil
}
func (e Executor) search(ctx context.Context, r Request) Result {
	out := Result{Version: Version, Code: "ok"}
	regex, _ := regexp.Compile(r.Pattern)
	pattern := r.Glob
	if pattern == "" {
		pattern = "**"
	}
	queue := []string{r.Path}
	visited := 0
	bytes := 0
	for len(queue) > 0 {
		if ctx.Err() != nil {
			out.Code = "canceled"
			return out
		}
		dir := queue[0]
		queue = queue[1:]
		entries, overflow, err := e.Files.ReadDir(ctx, dir, MaxScanEntries-visited)
		if err != nil {
			out.Code = errorCode(ctx, err)
			out.Denial = permission.Failure(err)
			return out
		}
		if overflow {
			out.Truncated = true
			return out
		}
		visited += len(entries)
		if visited >= MaxScanEntries {
			out.Truncated = true
			return out
		}
		for _, entry := range entries {
			relative := filepath.Join(dir, entry.Name)
			if entry.Directory {
				queue = append(queue, relative)
				continue
			}
			scope, _ := filepath.Rel(r.Path, relative)
			matches, _ := MatchGlob(pattern, scope)
			if !matches {
				continue
			}
			if r.Action == "glob" {
				if bytes+len(relative) > r.MaxBytes || len(out.Matches) >= r.Limit {
					out.Truncated = true
					return out
				}
				out.Matches = append(out.Matches, Match{Path: relative})
				bytes += len(relative)
				continue
			}
			snap, err := e.Files.Snapshot(ctx, relative)
			if err != nil {
				out.Skipped++
				continue
			}
			if !utf8.Valid(snap.Data) || strings.IndexByte(string(snap.Data), 0) >= 0 {
				out.Skipped++
				continue
			}
			for i, line := range strings.Split(string(snap.Data), "\n") {
				if !regex.MatchString(line) {
					continue
				}
				if len(out.Matches) >= r.Limit || bytes+len(relative) >= r.MaxBytes {
					out.Truncated = true
					return out
				}
				text, truncated := BoundText(line, r.MaxBytes-bytes-len(relative))
				out.Matches = append(out.Matches, Match{Path: relative, Line: i + 1, Text: text})
				bytes += len(relative) + len(text)
				if truncated {
					out.Truncated = true
					return out
				}
			}
		}
	}
	return out
}
func EncodeResult(result Result) []byte {
	encoded, err := json.Marshal(result)
	if err != nil {
		panic(fmt.Sprintf("encode native result: %v", err))
	}
	return encoded
}

func (e Executor) edit(ctx context.Context, id string, r Request) Result {
	identity, _ := json.Marshal(r)
	m := e.Files.ExecutePrepared(ctx, id, identity, func(ctx context.Context) (mutation.Request, error) {
		fail := func(code string) (mutation.Request, error) {
			return mutation.Request{}, mutation.PrepareFailure{Code: mutation.Code(code)}
		}
		snap, err := e.Files.Snapshot(ctx, r.Path)
		if err != nil {
			return mutation.Request{}, mutation.PrepareFailure{Code: mutation.Code(errorCode(ctx, err)), Denial: permission.Failure(err)}
		}
		if snap.Revision != *r.Expected {
			return fail("stale")
		}
		if !snap.Revision.Exists {
			return fail("not_found")
		}
		if !utf8.Valid(snap.Data) || strings.IndexByte(string(snap.Data), 0) >= 0 {
			return fail("binary")
		}
		count := strings.Count(string(snap.Data), r.OldText)
		if count == 0 {
			return fail("no_match")
		}
		if count > 1 && !r.ReplaceAll {
			return fail("ambiguous")
		}
		content := strings.ReplaceAll(string(snap.Data), r.OldText, r.NewText)
		return mutation.Request{Version: mutation.Version, Changes: []mutation.Change{{Path: r.Path, Expected: *r.Expected, Content: []byte(content)}}}, nil
	})
	return Result{Version: Version, Code: string(m.Code), Path: r.Path, Mutation: &m, Denial: m.Denial}
}
