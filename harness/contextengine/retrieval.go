package contextengine

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

type Hit struct {
	ID    string
	Score float64
	Exact int
}

// Retriever is disposable; a replacement receives exactly the same eligible
// units and deterministic query. No backend is allowed to mutate history.
type Retriever interface {
	Put(Unit)
	Remove(string)
	Search(query string, limit int, eligible ...func(string) bool) []Hit
}

type Lexical struct {
	postings map[string]map[string]struct{}
	terms    map[string][]string
	roots    map[string]string
}

func NewLexical() *Lexical {
	return &Lexical{postings: map[string]map[string]struct{}{}, terms: map[string][]string{}, roots: map[string]string{}}
}

// Terms retains filenames, symbols and error codes as well as their components.
// CJK bigrams allow useful matching without an external segmenter/embedding.
func Terms(text string) []string {
	set := map[string]bool{}
	add := func(t string) {
		if len(t) >= 2 && len(t) <= 128 {
			set[t] = true
		}
	}
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_.-/", r)
	})
	for _, f := range fields {
		add(f)
		for _, p := range strings.FieldsFunc(f, func(r rune) bool { return strings.ContainsRune("_.-/", r) }) {
			add(p)
		}
		runes := []rune(f)
		for i := 1; i < len(runes); i++ {
			if unicode.Is(unicode.Han, runes[i-1]) && unicode.Is(unicode.Han, runes[i]) {
				add(string(runes[i-1 : i+1]))
			}
		}
	}
	terms := make([]string, 0, len(set))
	for t := range set {
		terms = append(terms, t)
	}
	slices.Sort(terms)
	return terms
}

func (l *Lexical) Put(u Unit) {
	l.Remove(u.ID)
	if u.Class == Omit {
		return
	}
	ts := Terms(Text(u.Item))
	l.terms[u.ID] = ts
	l.roots[u.ID] = u.ID
	if u.ParentID != "" {
		l.roots[u.ID] = u.ParentID
	}
	for _, t := range ts {
		if l.postings[t] == nil {
			l.postings[t] = map[string]struct{}{}
		}
		l.postings[t][u.ID] = struct{}{}
	}
}
func (l *Lexical) Remove(id string) {
	for _, t := range l.terms[id] {
		delete(l.postings[t], id)
		if len(l.postings[t]) == 0 {
			delete(l.postings, t)
		}
	}
	delete(l.terms, id)
	delete(l.roots, id)
}
func (l *Lexical) Search(query string, limit int, eligible ...func(string) bool) []Hit {
	scores := map[string]float64{}
	exact := map[string]int{}
	identifiers := identifierTerms(query)
	for _, t := range Terms(query) {
		p := l.postings[t]
		weight := 1.0 / float64(max(1, len(p)))
		// Explicit code/path/symbol matches receive extra weight.
		if identifiers[t] {
			weight *= 4
		}
		for id := range p {
			scores[id] += weight
			if identifiers[t] {
				exact[id]++
			}
		}
	}
	hits := make([]Hit, 0, len(scores))
	for id, score := range scores {
		if len(eligible) > 0 && !eligible[0](id) {
			continue
		}
		hits = append(hits, Hit{ID: id, Score: score, Exact: exact[id]})
	}
	slices.SortFunc(hits, func(a, b Hit) int {
		if n := cmp.Compare(b.Exact, a.Exact); n != 0 {
			return n
		}
		// With the same exact identifier match, prefer the original canonical
		// source. Later questions repeating it must not outrank the evidence
		// merely by also repeating ordinary words in the current question.
		if a.Exact > 0 {
			if n := cmp.Compare(l.roots[a.ID], l.roots[b.ID]); n != 0 {
				return n
			}
		}
		if n := cmp.Compare(b.Score, a.Score); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return hits[:min(max(0, limit), len(hits))]
}

var quotedTerm = regexp.MustCompile("`([^`\\n]+)`|\"([^\"\\n]+)\"|'([^'\\n]+)'")

func identifierTerms(query string) map[string]bool {
	result := map[string]bool{}
	for _, term := range Terms(query) {
		if strings.ContainsAny(term, "_.-/0123456789") {
			result[term] = true
		}
	}
	for _, match := range quotedTerm.FindAllStringSubmatch(query, -1) {
		for _, text := range match[1:] {
			for _, term := range Terms(text) {
				result[term] = true
			}
		}
	}
	for _, word := range strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_.-/", r)
	}) {
		upper := 0
		for _, r := range word {
			if unicode.IsUpper(r) {
				upper++
			}
		}
		if upper >= 2 {
			result[strings.ToLower(word)] = true
		}
	}
	return result
}

// Selector is the replaceable selection boundary. Selection never writes a
// durable decision, generated summary, or reconstructed conversation.
type Selector interface {
	Select(SelectionInput) ([]Selected, error)
}
type SelectionInput struct {
	Pinned, Recent, Retrieved []Unit
	Budget, RecentReserve     int64
	RetrievalReserve          int64
	Retrieve                  func(retained []Selected, remaining int64) []Unit
	Cost                      func(Unit, string, bool) int64
}
type DeterministicSelector struct{}

func (DeterministicSelector) Select(in SelectionInput) ([]Selected, error) {
	var result []Selected
	seen := map[string]bool{}
	remaining := in.Budget
	add := func(u Unit, reason string, ref bool) bool {
		if seen[u.ID] {
			return true
		}
		if !ref && overlapsSource(u, result) {
			return false
		}
		cost := in.Cost(u, reason, ref)
		if cost > remaining {
			return false
		}
		remaining -= cost
		seen[u.ID] = true
		result = append(result, Selected{u, reason, ref})
		return true
	}
	for _, u := range in.Pinned {
		if !add(u, "pin", u.Class == Reference) && u.Required {
			return nil, &Error{"required_context_exceeds_budget"}
		}
	}
	// Reserve retrieval room, but never evict the latest fitting raw interaction.
	recentLeft := min(in.RecentReserve, max(0, remaining-min(in.RetrievalReserve, remaining/2)))
	for _, u := range in.Recent {
		if seen[u.ID] {
			continue
		}
		ref := u.Class == Reference
		cost := in.Cost(u, "recent", ref)
		if cost > recentLeft {
			continue
		}
		if add(u, "recent", ref) {
			recentLeft -= cost
		}
	}
	retrieved := in.Retrieved
	if in.Retrieve != nil {
		retrieved = in.Retrieve(result, remaining)
	}
	for _, u := range retrieved {
		add(u, "retrieved", false)
	}
	// Fill leftover budget with recent material, without duplicating evidence.
	for _, u := range in.Recent {
		add(u, "recent", u.Class == Reference)
	}
	return result, nil
}

func overlapsSelected(u Unit, selected []Selected) bool {
	for _, s := range selected {
		if s.ReferenceOnly {
			continue
		}
		if u.Fingerprint == s.Unit.Fingerprint && u.Fingerprint != "" {
			return true
		}
	}
	return overlapsSource(u, selected)
}

func overlapsSource(u Unit, selected []Selected) bool {
	for _, s := range selected {
		if s.ReferenceOnly {
			continue
		}
		if u.ParentID == s.Unit.ID || s.Unit.ParentID == u.ID {
			return true
		}
		if u.ParentID != "" && u.ParentID == s.Unit.ParentID && u.Source.OutputIndex == s.Unit.Source.OutputIndex && u.Source.StartByte < s.Unit.Source.EndByte && s.Unit.Source.StartByte < u.Source.EndByte {
			return true
		}
	}
	return false
}
