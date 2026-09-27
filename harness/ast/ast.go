// Package ast provides parser-backed structural queries and conditional rewrites.
package ast

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/mutation"
	"github.com/unreallabsai/unreal-agent/harness/native"
	"github.com/unreallabsai/unreal-agent/harness/permission"
)

const Version = 1
const MaxSourceBytes = 2 << 20
const MaxMatches = 1000

type Target struct {
	Path     string             `json:"path"`
	Expected *mutation.Revision `json:"expected,omitempty"`
}
type Request struct {
	Version         int      `json:"version"`
	Language        string   `json:"language"`
	Query           string   `json:"query"`
	Targets         []Target `json:"targets"`
	Replacement     *string  `json:"replacement,omitempty"`
	Apply           bool     `json:"apply,omitempty"`
	ExpectedMatches *int     `json:"expected_matches,omitempty"`
	MaxBytes        int      `json:"max_bytes,omitempty"`
}

func (r Request) Validate() error {
	if r.Version != Version {
		return errors.New("unsupported AST plan version")
	}
	switch r.Language {
	case "go", "javascript", "python":
	default:
		return Failure("unsupported_language")
	}
	if len(r.Query) == 0 || len(r.Query) > 16<<10 {
		return Failure("invalid_query")
	}
	if len(r.Targets) == 0 || len(r.Targets) > mutation.MaxTargets {
		return Failure("invalid_targets")
	}
	if r.MaxBytes < 0 || r.MaxBytes > native.MaxOutputBytes {
		return Failure("invalid_limit")
	}
	if r.Replacement != nil && len(*r.Replacement) > MaxSourceBytes {
		return Failure("replacement_too_large")
	}
	if r.Apply && (r.Replacement == nil || r.ExpectedMatches == nil || *r.ExpectedMatches <= 0 || *r.ExpectedMatches > MaxMatches) {
		return Failure("expected_match_count_required")
	}
	seen := map[string]bool{}
	for _, target := range r.Targets {
		if target.Path == "" || len(target.Path) > 4096 || seen[target.Path] || strings.IndexByte(target.Path, 0) >= 0 {
			return Failure("invalid_targets")
		}
		seen[target.Path] = true
		if target.Expected != nil {
			if target.Expected.Validate() != nil {
				return Failure("invalid_revision")
			}
		}
		if r.Apply && (target.Expected == nil || !target.Expected.Exists) {
			return Failure("expected_revision_required")
		}
	}
	return nil
}

type Failure string

func (e Failure) Error() string { return string(e) }

type Match struct {
	Start       int    `json:"start_byte"`
	End         int    `json:"end_byte"`
	Text        string `json:"text"`
	Replacement string `json:"replacement,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}
type File struct {
	Path     string            `json:"path"`
	Revision mutation.Revision `json:"revision"`
	Matches  []Match           `json:"matches"`
	Count    int               `json:"count"`
}
type Result struct {
	Version   int               `json:"version"`
	Code      string            `json:"code"`
	Count     int               `json:"count"`
	Files     []File            `json:"files,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
	Mutation  *mutation.Result  `json:"mutation,omitempty"`
	Denial    *permission.Error `json:"denial,omitempty"`
}
type analysis struct {
	matches []replacement
	content []byte
}
type replacement struct {
	start, end int
	text       string
}
type FileService interface {
	Snapshot(context.Context, string) (mutation.Snapshot, error)
	ExecutePrepared(context.Context, string, []byte, func(context.Context) (mutation.Request, error)) mutation.Result
}
type Executor struct{ Files FileService }

func (e Executor) Execute(ctx context.Context, id string, request Request) Result {
	result := Result{Version: Version, Code: "invalid"}
	if err := request.Validate(); err != nil {
		result.Code = err.Error()
		return result
	}
	if e.Files == nil {
		return result
	}
	if request.MaxBytes == 0 {
		request.MaxBytes = 16 << 10
	}
	if request.Apply {
		identity, _ := json.Marshal(request)
		m := e.Files.ExecutePrepared(ctx, id, identity, func(ctx context.Context) (mutation.Request, error) {
			changes, preview, err := e.prepare(ctx, request)
			if err != nil {
				return mutation.Request{}, mutation.PrepareFailure{Code: mutation.Code(code(err)), Denial: permission.Failure(err)}
			}
			if preview.Count != *request.ExpectedMatches {
				return mutation.Request{}, mutation.PrepareFailure{Code: "match_count_conflict"}
			}
			return mutation.Request{Version: mutation.Version, Changes: changes}, nil
		})
		result.Code = string(m.Code)
		result.Mutation = &m
		result.Denial = m.Denial
		if m.Code == mutation.Applied {
			result.Count = *request.ExpectedMatches
		}
		return result
	}
	_, result, err := e.prepare(ctx, request)
	if err != nil {
		result.Code = code(err)
		result.Denial = permission.Failure(err)
	}
	return result
}
func code(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	if permission.Failure(err) != nil {
		return "permission_denied"
	}
	var failure Failure
	if errors.As(err, &failure) {
		return string(failure)
	}
	return "read_failed"
}
func (e Executor) prepare(ctx context.Context, r Request) ([]mutation.Change, Result, error) {
	result := Result{Version: Version, Code: "ok"}
	changes := []mutation.Change{}
	budget := r.MaxBytes
	total := 0
	paths := map[string]bool{}
	for _, target := range r.Targets {
		if err := ctx.Err(); err != nil {
			return nil, result, err
		}
		snap, err := e.Files.Snapshot(ctx, target.Path)
		if err != nil {
			return nil, result, err
		}
		if paths[snap.Path] {
			return nil, result, Failure("duplicate_target")
		}
		paths[snap.Path] = true
		if !snap.Revision.Exists {
			return nil, result, Failure("not_found")
		}
		if target.Expected != nil && snap.Revision != *target.Expected {
			return nil, result, Failure("stale")
		}
		parsed, err := analyze(ctx, r.Language, r.Query, snap.Data, r.Replacement)
		if err != nil {
			return nil, result, err
		}
		result.Count += len(parsed.matches)
		if result.Count > MaxMatches {
			return nil, result, Failure("match_limit")
		}
		file := File{Path: target.Path, Revision: snap.Revision, Count: len(parsed.matches)}
		for _, match := range parsed.matches {
			if budget <= 0 {
				result.Truncated = true
				break
			}
			limit := min(budget, 512)
			before, bt := native.BoundText(string(snap.Data[match.start:match.end]), limit)
			budget -= len(before)
			after, at := native.BoundText(match.text, min(budget, 512))
			budget -= len(after)
			file.Matches = append(file.Matches, Match{Start: match.start, End: match.end, Text: before, Replacement: after, Truncated: bt || at})
			result.Truncated = result.Truncated || bt || at
		}
		result.Files = append(result.Files, file)
		if r.Replacement != nil {
			total += len(parsed.content)
			if total > mutation.MaxFileBytes {
				return nil, result, Failure("replacement_too_large")
			}
			// Include no-match files too: all target revisions are locked/preflighted.
			changes = append(changes, mutation.Change{Path: target.Path, Expected: snap.Revision, Content: parsed.content})
		}
	}
	if result.Count == 0 {
		result.Code = "no_match"
		if r.Apply {
			return nil, result, Failure("no_match")
		}
	}
	return changes, result, nil
}
