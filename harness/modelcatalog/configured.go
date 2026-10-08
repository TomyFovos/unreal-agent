package modelcatalog

import (
	"errors"
	"slices"
	"strings"
)

// Configured validates operator-supplied metadata. It performs no discovery or
// inference and adds neither models nor reasoning levels to the supplied list.
func Configured(models []Model) (Catalog, error) {
	c := Catalog{Source: "explicit operator configuration", Problem: "catalog unavailable"}
	if len(models) == 0 {
		return c, nil
	}
	if len(models) > 128 {
		return Catalog{}, errors.New("invalid configured model catalog")
	}
	seen := map[string]bool{}
	for _, m := range models {
		noEffort := m.SupportsEffort != nil && !*m.SupportsEffort
		if !validID(m.ID) || strings.HasPrefix(m.ID, "-") || seen[m.ID] || len(m.Name) > 256 || strings.ContainsAny(m.Name, "\x00\x1b\r\n") || m.ContextWindow < 0 || len(m.Efforts) == 0 && !noEffort || len(m.Efforts) > 5 || m.UnsupportedEfforts || noEffort && len(m.Efforts) != 0 {
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
		if m.Name == "" {
			m.Name = m.ID
		}
		m.Efforts = slices.Clone(m.Efforts)
		c.Models = append(c.Models, m)
	}
	c.Available, c.Problem = true, ""
	return c, nil
}
