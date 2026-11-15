package modelcatalog

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

// OpenAI reads only model IDs from the selected API endpoint. /models does not
// advertise reasoning levels. These are operator choices, sent unchanged to
// the API, which decides whether the selected model accepts them.
func OpenAI(ctx context.Context, endpoint string, key credential.Secret, client *http.Client) (Catalog, error) {
	fail := func() (Catalog, error) { return Catalog{}, errors.New("OpenAI API model catalog unavailable") }
	if key.Reveal() == "" {
		return fail()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/models", nil)
	if err != nil {
		return fail()
	}
	req.Header.Set("Authorization", "Bearer "+key.Reveal())
	hc := http.Client{}
	if client != nil {
		hc = *client
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Catalog{}, ctx.Err()
		}
		return fail()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Catalog{}, &credential.Error{Code: "api_key_rejected"}
	}
	if resp.StatusCode != http.StatusOK {
		return fail()
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.UnmarshalRead(resp.Body, &body) != nil {
		return fail()
	}
	c := Catalog{Source: "OpenAI API /models", Available: true, Authoritative: true, FetchedAt: time.Now()}
	seen := map[string]bool{}
	for _, row := range body.Data {
		if !validID(row.ID) || strings.HasPrefix(row.ID, "-") || seen[row.ID] || strings.Contains(row.ID, key.Reveal()) {
			continue
		}
		seen[row.ID] = true
		c.Models = append(c.Models, Model{ID: row.ID, Name: row.ID, Efforts: []llm.ReasoningEffort{"", llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortHigh, llm.ReasoningEffortXHigh, llm.ReasoningEffortMax}})
	}
	slices.SortFunc(c.Models, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	if len(c.Models) == 0 {
		return fail()
	}
	return c, nil
}
