//go:build cgo

package ast

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	ts "github.com/tree-sitter/go-tree-sitter"
	tsgo "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tsjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tspy "github.com/tree-sitter/tree-sitter-python/bindings/go"
)

const parserTimeout = 500 * time.Millisecond

// Runtime calls have a hard native timeout and a conservative success budget.
// This avoids accepting a timed-out partial query as a complete rewrite. v0.25
// callback APIs retain Go pointer payloads; use bounded synchronous APIs instead.
func parse(ctx context.Context, parser *ts.Parser, source []byte) (*ts.Tree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start := time.Now()
	parser.SetTimeoutMicros(uint64(parserTimeout / time.Microsecond))
	tree := parser.Parse(source, nil)
	if err := ctx.Err(); err != nil {
		if tree != nil {
			tree.Close()
		}
		return nil, err
	}
	if tree == nil || time.Since(start) >= parserTimeout/2 {
		if tree != nil {
			tree.Close()
		}
		return nil, Failure("parse_budget_exceeded")
	}
	if tree.RootNode().HasError() {
		tree.Close()
		return nil, Failure("malformed_source")
	}
	return tree, nil
}
func analyze(ctx context.Context, language, sourceQuery string, source []byte, template *string) (analysis, error) {
	if len(source) > MaxSourceBytes {
		return analysis{}, Failure("source_too_large")
	}
	if !utf8.Valid(source) || strings.IndexByte(string(source), 0) >= 0 {
		return analysis{}, Failure("binary_source")
	}
	var grammar *ts.Language
	switch language {
	case "go":
		grammar = ts.NewLanguage(tsgo.Language())
	case "javascript":
		grammar = ts.NewLanguage(tsjs.Language())
	case "python":
		grammar = ts.NewLanguage(tspy.Language())
	default:
		return analysis{}, Failure("unsupported_language")
	}
	parser := ts.NewParser()
	defer parser.Close()
	if parser.SetLanguage(grammar) != nil {
		return analysis{}, Failure("unsupported_grammar")
	}
	tree, err := parse(ctx, parser, source)
	if err != nil {
		return analysis{}, err
	}
	defer tree.Close()
	query, queryErr := ts.NewQuery(grammar, sourceQuery)
	if queryErr != nil {
		return analysis{}, Failure("invalid_query")
	}
	defer query.Close()
	matchID, ok := query.CaptureIndexForName("match")
	if !ok {
		return analysis{}, Failure("match_capture_required")
	}
	names := query.CaptureNames()
	if len(names) > 32 || query.PatternCount() > 16 {
		return analysis{}, Failure("query_too_large")
	}
	for i := uint(0); i < query.PatternCount(); i++ {
		if len(query.GeneralPredicates(i))+len(query.PropertyPredicates(i))+len(query.PropertySettings(i)) != 0 {
			return analysis{}, Failure("unsupported_predicate")
		}
	}
	cursor := ts.NewQueryCursor()
	defer cursor.Close()
	cursor.SetMatchLimit(MaxMatches)
	cursor.SetTimeoutMicros(uint64(parserTimeout / time.Microsecond))
	started := time.Now()
	matches := cursor.Matches(query, tree.RootNode(), source)
	out := analysis{}
	replacementBytes := 0
	for {
		if err := ctx.Err(); err != nil {
			return analysis{}, err
		}
		match := matches.Next()
		if match == nil {
			break
		}
		if len(out.matches) >= MaxMatches {
			return analysis{}, Failure("match_limit")
		}
		type span struct{ start, end uint }
		captures := map[string]span{}
		found := false
		item := replacement{}
		for _, capture := range match.Captures {
			name := names[capture.Index]
			current := span{capture.Node.StartByte(), capture.Node.EndByte()}
			if existing, ok := captures[name]; ok && existing != current {
				return analysis{}, Failure("ambiguous_capture")
			}
			captures[name] = current
			if uint(capture.Index) == matchID {
				if found {
					return analysis{}, Failure("ambiguous_capture")
				}
				found = true
				item.start = int(current.start)
				item.end = int(current.end)
			}
		}
		if !found || item.end <= item.start {
			return analysis{}, Failure("ambiguous_capture")
		}
		if template != nil {
			item.text, err = expand(*template, func(name string) (string, bool) {
				span, ok := captures[name]
				if !ok {
					return "", false
				}
				return string(source[span.start:span.end]), true
			})
			if err != nil {
				return analysis{}, err
			}
		}
		replacementBytes += len(item.text)
		if replacementBytes > MaxSourceBytes {
			return analysis{}, Failure("replacement_too_large")
		}
		out.matches = append(out.matches, item)
	}
	if time.Since(started) >= parserTimeout/2 {
		return analysis{}, Failure("query_budget_exceeded")
	}
	if cursor.DidExceedMatchLimit() {
		return analysis{}, Failure("match_limit")
	}
	slices.SortFunc(out.matches, func(a, b replacement) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return a.end - b.end
	})
	for i := 1; i < len(out.matches); i++ {
		if out.matches[i].start < out.matches[i-1].end {
			return analysis{}, Failure("overlapping_matches")
		}
	}
	if template != nil {
		var builder strings.Builder
		offset := 0
		for _, change := range out.matches {
			if builder.Len()+change.start-offset+len(change.text) > MaxSourceBytes {
				return analysis{}, Failure("replacement_too_large")
			}
			builder.Write(source[offset:change.start])
			builder.WriteString(change.text)
			offset = change.end
		}
		if builder.Len()+len(source)-offset > MaxSourceBytes {
			return analysis{}, Failure("replacement_too_large")
		}
		builder.Write(source[offset:])
		out.content = []byte(builder.String())
		replacementTree, err := parse(ctx, parser, out.content)
		if err != nil {
			if err == Failure("malformed_source") {
				return analysis{}, Failure("malformed_replacement")
			}
			return analysis{}, err
		}
		replacementTree.Close()
	}
	return out, nil
}
func expand(template string, capture func(string) (string, bool)) (string, error) {
	var out strings.Builder
	for i := 0; i < len(template); {
		if out.Len() > MaxSourceBytes {
			return "", Failure("replacement_too_large")
		}
		if template[i] != '$' {
			out.WriteByte(template[i])
			i++
			continue
		}
		if i+1 < len(template) && template[i+1] == '$' {
			out.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(template) || template[i+1] != '{' {
			out.WriteByte('$')
			i++
			continue
		}
		end := strings.IndexByte(template[i+2:], '}')
		if end < 0 {
			return "", Failure("invalid_template")
		}
		name := template[i+2 : i+2+end]
		text, ok := capture(name)
		if !ok {
			return "", Failure("unknown_capture")
		}
		out.WriteString(text)
		i += end + 3
	}
	if out.Len() > MaxSourceBytes {
		return "", Failure("replacement_too_large")
	}
	return out.String(), nil
}
