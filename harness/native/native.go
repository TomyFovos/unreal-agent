// Package native executes the bounded file tools. Translators only encode plans.
package native

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/permission"
	"github.com/unreallabsai/unreal-agent/harness/record"
)

const Version = 1

type Request struct {
	Version      int                `json:"version"`
	Action       string             `json:"action"`
	Path         string             `json:"path"`
	Expected     *mutation.Revision `json:"expected,omitempty"`
	Content      string             `json:"content,omitempty"`
	OldText      string             `json:"old_text,omitempty"`
	NewText      string             `json:"new_text,omitempty"`
	ReplaceAll   bool               `json:"replace_all,omitempty"`
	Pattern      string             `json:"pattern,omitempty"`
	Glob         string             `json:"glob,omitempty"`
	StartLine    int                `json:"start_line,omitempty"`
	Lines        int                `json:"lines,omitempty"`
	Files        []ReadFile         `json:"files,omitempty"`
	ContextLines int                `json:"context_lines,omitempty"`
	Limit        int                `json:"limit,omitempty"`
	MaxBytes     int                `json:"max_bytes,omitempty"`
}

func (r Request) Validate() error {
	if r.Version != Version {
		return errors.New("unsupported native plan version")
	}
	if (r.Action != "read" || r.Files == nil) && !validPath(r.Path) {
		return errors.New("path is required")
	}
	if r.MaxBytes < 0 || r.Limit < 0 || r.StartLine < 0 || r.Lines < 0 || r.ContextLines < 0 {
		return errors.New("range/output limit out of bounds")
	}
	if r.Files != nil && r.Action != "read" || r.ContextLines != 0 && r.Action != "grep" {
		return errors.New("files applies to read and context_lines applies to grep")
	}
	switch r.Action {
	case "read":
		if r.Files != nil {
			if len(r.Files) == 0 || r.Path != "" || r.StartLine != 0 || r.Lines != 0 {
				return errors.New("specify path with an optional range, or a nonempty files array")
			}
			for _, file := range r.Files {
				if !validPath(file.Path) {
					return errors.New("each file requires a path")
				}
				for _, lines := range file.Ranges {
					if lines.StartLine < 0 || lines.Lines < 0 {
						return errors.New("file range out of bounds")
					}
				}
			}
		}
	case "write", "edit":
		if r.Expected == nil {
			return errors.New("expected revision is required")
		}
		if err := r.Expected.Validate(); err != nil {
			return err
		}
		if r.Action == "edit" && r.OldText == "" {
			return errors.New("old_text must be nonempty")
		}
	case "grep":
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return fmt.Errorf("compile regular expression: %w", err)
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

func validPath(path string) bool { return path != "" && strings.IndexByte(path, 0) < 0 }

type Match struct {
	Path   string   `json:"path"`
	Line   int      `json:"line,omitempty"`
	Text   string   `json:"text,omitempty"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}
type Result struct {
	Denial    *permission.Error  `json:"denial,omitempty"`
	Version   int                `json:"version"`
	Code      string             `json:"code"`
	Reason    mutation.Reason    `json:"reason,omitempty"`
	Path      string             `json:"path,omitempty"`
	Revision  *mutation.Revision `json:"revision,omitempty"`
	Content   string             `json:"content,omitempty"`
	StartLine int                `json:"start_line,omitempty"`
	Binary    bool               `json:"binary,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
	Matches   []Match            `json:"matches,omitempty"`
	Skipped   int                `json:"skipped,omitempty"`
	Mutation  *mutation.Result   `json:"mutation,omitempty"`
	Files     []ReadFileResult   `json:"files,omitempty"`
	Locator   record.Locator     `json:"locator,omitempty"`
}
type Executor struct{ Files *mutation.Service }

func (e Executor) Execute(ctx context.Context, id string, r Request) Result {
	result := Result{Version: Version, Code: "invalid"}
	if r.Validate() != nil || e.Files == nil {
		result.Reason = mutation.ReasonInvalidRequest
		if r.Expected != nil && r.Expected.Validate() != nil {
			result.Reason = mutation.ReasonInvalidRevision
		}
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
	if r.Files != nil {
		return e.readBatch(ctx, r)
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
		result.Reason = m.Reason
		return result
	}
	result, text := e.readFile(ctx, r.Path)
	if result.Code == "ok" {
		part := readRange(strings.SplitAfter(text, "\n"), LineRange{StartLine: r.StartLine, Lines: r.Lines}, r.MaxBytes)
		result.StartLine, result.Content, result.Truncated = part.StartLine, part.Content, part.Truncated
	}
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
	parts := strings.Split(pattern, "/")
	names := strings.Split(path, "/")
	for _, part := range parts {
		if part != "**" {
			if _, err := filepath.Match(part, ""); err != nil {
				return false, fmt.Errorf("invalid glob: %w", err)
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
	if r.ContextLines > 0 {
		r.MaxBytes = min(r.MaxBytes, maxFileOutputBytes)
	}
	regex, _ := regexp.Compile(r.Pattern)
	pattern := r.Glob
	if pattern == "" {
		pattern = "**"
	}
	queue := []string{r.Path}
	bytes := 0
	for len(queue) > 0 {
		if ctx.Err() != nil {
			out.Code = "canceled"
			return out
		}
		dir := queue[0]
		queue = queue[1:]
		entries, err := e.Files.ReadDir(ctx, dir)
		if err != nil {
			out.Code = errorCode(ctx, err)
			out.Denial = permission.Failure(err)
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
			text := string(snap.Data)
			if r.ContextLines > 0 {
				text = strings.TrimSuffix(text, "\n")
			}
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				if !regex.MatchString(line) {
					continue
				}
				if len(out.Matches) >= r.Limit || bytes+len(relative) >= r.MaxBytes {
					out.Truncated = true
					return out
				}
				remaining := r.MaxBytes - bytes - len(relative)
				text, truncated := BoundText(line, remaining)
				match := Match{Path: relative, Line: i + 1, Text: text}
				remaining -= len(text)
				if !truncated && r.ContextLines > 0 {
					before, cutBefore := grepContext(lines[max(0, i-r.ContextLines):i], true, &remaining)
					after, cutAfter := grepContext(lines[i+1:i+1+min(r.ContextLines, len(lines)-i-1)], false, &remaining)
					match.Before, match.After = before, after
					truncated = cutBefore || cutAfter
				}
				out.Matches = append(out.Matches, match)
				bytes = r.MaxBytes - remaining
				if truncated {
					out.Truncated = true
					return out
				}
			}
		}
	}
	return out
}

func grepContext(lines []string, backwards bool, remaining *int) ([]string, bool) {
	var out []string
	truncated := false
	for i := range lines {
		if *remaining == 0 {
			truncated = true
			break
		}
		index := i
		if backwards {
			index = len(lines) - 1 - i
		}
		text, cut := BoundText(lines[index], *remaining-1)
		*remaining -= len(text) + 1 // count a line separator, including for empty lines
		out = append(out, text)
		if cut {
			truncated = true
			break
		}
	}
	if backwards {
		slices.Reverse(out)
	}
	return out, truncated
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
			reason := mutation.Reason("")
			if code == "stale" {
				reason = mutation.ReasonRevisionConflict
			}
			return mutation.Request{}, mutation.PrepareFailure{Code: mutation.Code(code), Reason: reason}
		}
		snap, err := e.Files.SnapshotForMutation(ctx, r.Path)
		if err != nil {
			return mutation.Request{}, mutation.PrepareFailure{Code: mutation.Code(errorCode(ctx, err)), Denial: permission.Failure(err), Reason: mutation.FailureReason(err)}
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
	return Result{Version: Version, Code: string(m.Code), Reason: m.Reason, Path: r.Path, Mutation: &m, Denial: m.Denial}
}
