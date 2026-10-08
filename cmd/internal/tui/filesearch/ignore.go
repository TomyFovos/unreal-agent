package filesearch

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type ignoreRule struct {
	base, pattern               string
	negate, directory, anchored bool
}

func readRules(root *os.Root, name, base string, limit int) ([]ignoreRule, int, error) {
	info, err := root.Lstat(filepath.FromSlash(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, 0, errIgnore
	}
	f, err := openIgnore(root, filepath.FromSlash(name))
	if err != nil {
		return nil, 0, errIgnore
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, 0, errIgnore
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(data) > limit {
		return nil, 0, errIgnore
	}
	rules, err := parseRules(string(data), base)
	return rules, len(data), err
}

// Git-style hierarchy/last-match/negation, anchored and directory patterns,
// escaped comments/spaces, component globbing and **. An ignored directory is
// never traversed, so negation cannot resurrect a child of an excluded parent.
func parseRules(data, base string) ([]ignoreRule, error) {
	var rules []ignoreRule
	for _, raw := range strings.Split(data, "\n") {
		s := strings.TrimSuffix(raw, "\r")
		for strings.HasSuffix(s, " ") {
			backslashes := 0
			for i := len(s) - 2; i >= 0 && s[i] == '\\'; i-- {
				backslashes++
			}
			if backslashes%2 != 0 {
				break
			}
			s = strings.TrimSuffix(s, " ")
		}
		if s == "" || s[0] == '#' {
			continue
		}
		r := ignoreRule{base: base}
		if s[0] == '!' {
			r.negate = true
			s = s[1:]
		}
		r.directory = strings.HasSuffix(s, "/")
		s = strings.TrimSuffix(s, "/")
		r.anchored = strings.HasPrefix(s, "/") || strings.Contains(s, "/")
		s = strings.TrimPrefix(s, "/")
		if s == "" || len(s) > 4096 {
			return nil, errIgnore
		}
		for _, component := range strings.Split(s, "/") {
			if component == ".." {
				return nil, errIgnore
			}
			if _, err := path.Match(component, ""); err != nil {
				return nil, errIgnore
			}
		}
		r.pattern = s
		rules = append(rules, r)
	}
	return rules, nil
}

func ignored(rules []ignoreRule, name string, directory bool) bool {
	decision := false
	for _, r := range rules {
		rel := name
		if r.base != "." {
			if !strings.HasPrefix(name, r.base+"/") {
				continue
			}
			rel = strings.TrimPrefix(name, r.base+"/")
		}
		if r.directory && !directory {
			continue
		}
		matched := false
		if r.anchored {
			matched = globPath(r.pattern, rel)
		} else {
			matched, _ = path.Match(r.pattern, path.Base(rel))
		}
		if matched {
			decision = !r.negate
		}
	}
	return decision
}

func globPath(pattern, name string) bool {
	patterns, parts := strings.Split(pattern, "/"), strings.Split(name, "/")
	// Dynamic programming keeps ** patterns bounded, including adversarial
	// repeated wildcards; there is no exponentially recursive matching.
	previous := make([]bool, len(parts)+1)
	previous[0] = true
	for _, p := range patterns {
		next := make([]bool, len(parts)+1)
		if p == "**" {
			next[0] = previous[0]
			for j := 1; j <= len(parts); j++ {
				next[j] = previous[j] || next[j-1]
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				match, _ := path.Match(p, parts[j-1])
				next[j] = previous[j-1] && match
			}
		}
		previous = next
	}
	return previous[len(parts)]
}
