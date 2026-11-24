package claudeagent

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
)

// With Claude Code 2.1.311/Haiku, this list reduced first-call input from
// 23,249 to 19,350 tokens (3,899 saved), exceeding the approved 2,000 cutoff.
const enabledTools = "Bash,Read,Edit,Write,Glob,Grep,WebFetch,WebSearch,NotebookEdit,TodoWrite,ToolSearch,Skill"

const disallowedTools = "Agent,Task"

// Claude Code reads the workspace's instructions itself. Only existing files
// above that root are excluded; the user's global instructions remain enabled.
func ancestorInstructionSettings(directory string, env []string) string {
	directory = filepath.Clean(directory)
	home, _ := os.UserHomeDir()
	for _, kv := range env {
		if key, value, _ := strings.Cut(kv, "="); key == "HOME" {
			home = value
		}
	}
	global := filepath.Join(home, ".claude", "CLAUDE.md")
	var excludes []string
	for parent := filepath.Dir(directory); parent != directory; parent = filepath.Dir(directory) {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md", "CLAUDE.local.md"} {
			path := filepath.Join(parent, name)
			if path == global {
				continue
			}
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				excludes = append(excludes, path)
			}
		}
		directory = parent
	}
	if len(excludes) == 0 {
		return ""
	}
	data, _ := json.Marshal(struct {
		Excludes []string `json:"claudeMdExcludes"`
	}{excludes})
	return string(data)
}
