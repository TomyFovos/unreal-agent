package viewer

import (
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"

	"github.com/rivo/uniseg"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/internal/secretguard"
)

var shellEnvironmentAssignment = regexp.MustCompile(`(?:^|\s)[A-Za-z_][A-Za-z0-9_]*=`)

// SafeToolTarget projects only the display arguments used by Live Dock. Opaque
// tool/Operation state, environment, output and unrecognized fields stay out.
func SafeToolTarget(c llm.ToolCall) string {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Glob    string `json:"glob"`
		Command string `json:"command"`
		Action  string `json:"action"`
		Query   string `json:"query"`
		Name    string `json:"name"`
		Task    string `json:"task"`
		Source  string `json:"source"`
		Targets []struct {
			Path string `json:"path"`
		} `json:"targets"`
		Start *struct {
			Program string `json:"program"`
		} `json:"start"`
	}
	if len(c.Arguments) > 1<<20 || json.Unmarshal([]byte(c.Arguments), &a) != nil {
		return ""
	}
	var target string
	switch c.Name {
	case "read", "write", "edit", "ViewImage":
		target = a.Path
	case "grep":
		target = fmt.Sprintf("%q %s", a.Pattern, a.Path)
	case "glob":
		target = a.Glob
	case "Bash":
		target = strings.SplitN(a.Command, "\n", 2)[0]
		if shellEnvironmentAssignment.MatchString(target) || strings.HasPrefix(strings.TrimSpace(target), "env ") {
			return ""
		}
	case "ast_grep", "ast_edit":
		if len(a.Targets) > 0 {
			target = a.Targets[0].Path
		}
	case "LSP":
		target = a.Path
		if target == "" {
			target = a.Query
		}
		target = strings.TrimSpace(a.Action + " " + target)
	case "DAP":
		target = a.Source
		if target == "" && a.Start != nil {
			target = a.Start.Program
		}
		target = strings.TrimSpace(a.Command + " " + target)
	case "SkillUse":
		target = a.Name
	case "SubagentStart":
		target = a.Task
	case "claude_code":
		// A call Claude Code ran itself: show its tool and what it acted on.
		var o struct {
			Tool  string `json:"tool"`
			Input struct {
				FilePath     string `json:"file_path"`
				NotebookPath string `json:"notebook_path"`
				Command      string `json:"command"`
				URL          string `json:"url"`
				Pattern      string `json:"pattern"`
				Path         string `json:"path"`
				Description  string `json:"description"`
			} `json:"input"`
		}
		if json.Unmarshal([]byte(c.Arguments), &o) != nil {
			return ""
		}
		detail := o.Input.FilePath
		for _, v := range []string{o.Input.NotebookPath, o.Input.URL, o.Input.Pattern, o.Input.Path, o.Input.Description} {
			if detail == "" {
				detail = v
			}
		}
		if command := strings.SplitN(o.Input.Command, "\n", 2)[0]; command != "" {
			if shellEnvironmentAssignment.MatchString(command) || strings.HasPrefix(strings.TrimSpace(command), "env ") {
				return o.Tool
			}
			detail = command
		}
		target = strings.TrimSpace(o.Tool + " " + detail)
	}
	// Never expose credential-shaped display arguments, even within a recognized
	// field. Check before clipping so truncation cannot hide a sensitive prefix.
	if secretguard.Sensitive(target) {
		return ""
	}
	target = strings.Join(strings.Fields(SafeText(target)), " ")
	if secretguard.Sensitive(target) {
		return ""
	}
	g := uniseg.NewGraphemes(target)
	end := 0
	for g.Next() {
		_, next := g.Positions()
		if next > 4096 {
			break
		}
		end = next
	}
	return target[:end]
}
