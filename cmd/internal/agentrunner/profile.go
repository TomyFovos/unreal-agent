package agentrunner

import (
	"github.com/unreallabsai/unreal-agent/harness/profile"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

func resolveRequestProfile(parsed Request, selected provider.Selection) (profile.Resolved, error) {
	selection := profile.Default()
	if parsed.ProfileID != "" || parsed.ProfileVersion != 0 {
		selection = profile.Selection{ID: parsed.ProfileID, Version: parsed.ProfileVersion, Source: "request"}
	}
	selected.Model.Family = parsed.ModelFamily
	return profile.Resolve(selection, selected)
}
