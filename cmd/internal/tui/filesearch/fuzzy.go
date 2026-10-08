package filesearch

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ResultLimit = 32
const QueryLimit = 256 // UTF-8 bytes, independent of the composer size limit
type Match struct{ Path string }
type ranked struct {
	Path                string
	rank, score, length int
}

// Filename-first ranking and bounded sorted top-N follow upstream's search
// design. The subsequence scorer is local, so no fuzzy/UI dependencies are added.
func (index Index) Search(ctx context.Context, query string, limit int) ([]Match, error) {
	if len(query) > QueryLimit || !utf8.ValidString(query) {
		return nil, nil
	}
	query = strings.TrimPrefix(query, "./")
	limit = min(limit, ResultLimit)
	if limit <= 0 {
		return nil, nil
	}
	best := make([]ranked, 0, limit)
	compare := func(a, b ranked) int {
		if n := cmp.Compare(b.rank, a.rank); n != 0 {
			return n
		}
		if n := cmp.Compare(b.score, a.score); n != 0 {
			return n
		}
		if n := cmp.Compare(a.length, b.length); n != 0 {
			return n
		}
		return strings.Compare(a.Path, b.Path)
	}
	for _, name := range index.files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r := ranked{Path: name, length: utf8.RuneCountInString(name)}
		if query == "" {
			r.score = -r.length
		} else {
			source := name
			if !strings.Contains(query, "/") {
				filename := name[strings.LastIndexByte(name, '/')+1:]
				if score, ok := fuzzyScore(query, filename); ok {
					r.rank, r.score, source = 1, score, ""
					if strings.EqualFold(query, filename) {
						r.rank = 2
					}
				}
			}
			if source != "" {
				score, ok := fuzzyScore(query, source)
				if !ok {
					continue
				}
				r.score = score
			}
		}
		position, _ := slices.BinarySearchFunc(best, r, compare)
		if position >= limit {
			continue
		}
		best = slices.Insert(best, position, r)
		if len(best) > limit {
			best = best[:limit]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matches := make([]Match, len(best))
	for i, r := range best {
		matches[i] = Match{Path: r.Path}
	}
	return matches, nil
}

func fuzzyScore(query, value string) (int, bool) {
	q, v := []rune(query), []rune(value)
	position, previous, score := 0, -2, 0
	for _, wanted := range q {
		found := -1
		for position < len(v) {
			if unicode.ToLower(wanted) == unicode.ToLower(v[position]) {
				found = position
				position++
				break
			}
			position++
		}
		if found < 0 {
			return 0, false
		}
		score += 10
		if found == previous+1 {
			score += 8
		}
		if found == 0 || strings.ContainsRune("/_.- ", v[found-1]) {
			score += 6
		}
		if wanted == v[found] {
			score++
		}
		if previous < 0 {
			score -= found
		} else {
			score -= found - previous - 1
		}
		previous = found
	}
	return score, true
}
