package modelcatalog

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/buildfeatures"
)

const openCodeGoSource = "OpenCode Go model list"

// The list was 33 KB on 2026-11-03; this only bounds a broken response.
const maxOpenCodeGoList = 4 << 20

// ReadOpenCodeGo reads the public OpenCode Go model list. It sends no key and
// follows no redirect. Failures are reported in the Catalog, as for Codex.
func ReadOpenCodeGo(ctx context.Context, client *http.Client, url string, offered []string) Catalog {
	if !buildfeatures.OpenCodeGoEnabled {
		return openCodeGoFailure(buildfeatures.OpenCodeGoDisabledCode)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return openCodeGoFailure("opencode_go_list_invalid")
	}
	request.Header.Set("User-Agent", "unreal-agent")
	guarded := *client
	guarded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := guarded.Do(request)
	if err != nil {
		return openCodeGoFailure("opencode_go_list_unreachable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return openCodeGoFailure("opencode_go_list_status")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOpenCodeGoList+1))
	if err != nil || len(body) > maxOpenCodeGoList {
		return openCodeGoFailure("opencode_go_list_unreadable")
	}
	c := ParseOpenCodeGo(body, offered)
	if c.Available {
		c.FetchedAt = time.Now()
	}
	return c
}

// ParseOpenCodeGo keeps only the offered models, in their offered order
// (docs/opencode-go.md). The list gives no default effort, so high is
// used where the model has it.
func ParseOpenCodeGo(data []byte, offered []string) Catalog {
	if !buildfeatures.OpenCodeGoEnabled {
		return openCodeGoFailure(buildfeatures.OpenCodeGoDisabledCode)
	}
	var list struct {
		Data []jsontext.Value `json:"data"`
	}
	if json.Unmarshal(data, &list) != nil {
		return openCodeGoFailure("opencode_go_list_invalid")
	}
	found := map[string]Model{}
	for _, item := range list.Data {
		var head struct {
			ID string `json:"id"`
		}
		// Other models' entries are not read, so their format cannot break this one.
		if json.Unmarshal(item, &head) != nil || !slices.Contains(offered, head.ID) {
			continue
		}
		var raw struct {
			Name         string `json:"name"`
			Description  string `json:"description"`
			Capabilities struct {
				Reasoning struct {
					Supported bool     `json:"supported"`
					Effort    []string `json:"effort"`
				} `json:"reasoning"`
			} `json:"capabilities"`
			Limit struct {
				Context int64 `json:"context"`
			} `json:"limit"`
		}
		if json.Unmarshal(item, &raw) != nil {
			return openCodeGoFailure("opencode_go_list_invalid")
		}
		m := Model{ID: head.ID, Name: displayable(raw.Name, head.ID), Description: displayable(raw.Description, ""), ContextWindow: raw.Limit.Context}
		for _, value := range raw.Capabilities.Reasoning.Effort {
			if e := llm.ReasoningEffort(value); e.Valid() && e != llm.ReasoningEffortUltra && !slices.Contains(m.Efforts, e) {
				m.Efforts = append(m.Efforts, e)
			}
		}
		switch {
		case !raw.Capabilities.Reasoning.Supported:
			m.SupportsEffort, m.Efforts = new(false), nil
		case len(m.Efforts) == 0:
			m.EffortUnknown = true
		case slices.Contains(m.Efforts, llm.ReasoningEffortHigh):
			m.DefaultEffort = llm.ReasoningEffortHigh
		default:
			m.DefaultEffort = m.Efforts[0]
		}
		found[head.ID] = m
	}
	c := Catalog{Source: openCodeGoSource}
	for _, id := range offered {
		if m, ok := found[id]; ok {
			c.Models = append(c.Models, m)
		}
	}
	if len(c.Models) == 0 {
		return openCodeGoFailure("opencode_go_model_missing")
	}
	c.Available = true
	return c
}

func openCodeGoFailure(code string) Catalog {
	return Catalog{Source: openCodeGoSource, Problem: "catalog unavailable", ProblemCode: code}
}

// displayable keeps remote text out of the terminal when it carries control
// characters.
func displayable(text, fallback string) string {
	if strings.IndexFunc(text, unicode.IsControl) >= 0 || text == "" {
		return fallback
	}
	return text
}
