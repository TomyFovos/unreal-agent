package tui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DropDirectory is where files sent from Windows land in a sandbox
// (scripts/windows/sandbox-drop.cmd).
func DropDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "drop")
}

var windowsPath = regexp.MustCompile(`"([A-Za-z]:\\[^"\r\n]+)"|'([A-Za-z]:\\[^'\r\n]+)'|([A-Za-z]:\\\S+)`)

// mapDroppedPaths turns Windows paths a terminal pasted for dragged files
// into the paths of the same files in the drop directory, when they are
// there. It reports the names it could not find.
func mapDroppedPaths(text, drop string) (string, []string) {
	if drop == "" || !windowsPath.MatchString(text) {
		return text, nil
	}
	var missing []string
	mapped := windowsPath.ReplaceAllStringFunc(text, func(match string) string {
		m := windowsPath.FindStringSubmatch(match)
		path := m[1] + m[2] + m[3]
		name := path[strings.LastIndexByte(path, '\\')+1:]
		local := filepath.Join(drop, name)
		if name == "" {
			return match
		}
		if info, err := os.Stat(local); err != nil || info.IsDir() {
			missing = append(missing, name)
			return match
		}
		if strings.ContainsAny(local, " \t'\"") {
			return `"` + local + `"`
		}
		return local
	})
	return mapped, missing
}
