package host

import (
	"runtime/debug"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func hostStartRecord(startedAt time.Time) llm.ActivityRecord {
	r := llm.ActivityRecord{Kind: "host_start", StartedAt: startedAt, Revision: "unknown"}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				r.Revision = setting.Value
			case "vcs.modified":
				r.Modified = setting.Value == "true"
			}
		}
	}
	return r
}
