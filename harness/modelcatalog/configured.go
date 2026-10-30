package modelcatalog

import (
	"errors"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// Configured validates operator-supplied metadata. It performs no discovery or
// inference and adds neither models nor reasoning levels to the supplied list.
func Configured(models []Model) (Catalog, error) {
	c := Catalog{Source: "explicit operator configuration", Problem: "catalog unavailable"}
	if len(models) == 0 {
		return c, nil
	}
	seen := map[string]bool{}
	for _, m := range models {
		noEffort := m.SupportsEffort != nil && !*m.SupportsEffort
		if !validID(m.ID) || strings.HasPrefix(m.ID, "-") || seen[m.ID] || strings.ContainsAny(m.Name, "\x00\x1b\r\n") || m.ContextWindow < 0 || len(m.Efforts) == 0 && !noEffort || m.UnsupportedEfforts || noEffort && len(m.Efforts) != 0 {
			return Catalog{}, errors.New("invalid configured model catalog")
		}
		seen[m.ID] = true
		for i, e := range m.Efforts {
			if !e.Valid() || slices.Contains(m.Efforts[:i], e) {
				return Catalog{}, errors.New("invalid configured model effort")
			}
		}
		if m.DefaultEffort != "" && !slices.Contains(m.Efforts, m.DefaultEffort) {
			return Catalog{}, errors.New("invalid configured default effort")
		}
		// These are capabilities, not saved selections: do not invent max when
		// filtering an old catalog that advertises only lower levels and ultra.
		m.Efforts = slices.DeleteFunc(slices.Clone(m.Efforts), func(e llm.ReasoningEffort) bool { return e == llm.ReasoningEffortUltra })
		m.DefaultEffort = m.RequestEffort(m.DefaultEffort)
		if m.Name == "" {
			m.Name = m.ID
		}
		c.Models = append(c.Models, m)
	}
	c.Available, c.Problem = true, ""
	return c, nil
}
