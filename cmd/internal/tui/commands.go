package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
)

type commandArguments uint8

const (
	noArguments commandArguments = iota
	optionalArguments
	requiredArguments
)

type commandCandidate struct {
	Name, Usage, Description string
	Args                     commandArguments
}

var commandCatalog = []commandCandidate{
	{"/stop", "/stop", "hard stop this session", noArguments},
	{"/stop idle", "/stop idle", "stop when the agent is idle", noArguments},
	{"/resume", "/resume", "resume this stopped session", noArguments},
	{"/retry", "/retry", "resend the last uncertain input (same input ID)", noArguments},
	{"/detach", "/detach", "leave; the session keeps running", noArguments},
	{"/login", "/login PROVIDER ID", "store an API key (masked entry)", requiredArguments},
	{"/logout", "/logout PROVIDER ID", "remove a stored credential", requiredArguments},
	{"/credentials", "/credentials", "list stored credentials", noArguments},
	{"/methods", "/methods", "show supported authentication methods", noArguments},
	{"/children", "/children", "how to select a child", noArguments},
	{"/child", "/child ID", "focus a child agent", requiredArguments},
	{"/child-history", "/child-history [after] [limit]", "load a page of its history", optionalArguments},
	{"/child-next", "/child-next", "load the next history page", noArguments},
	{"/child-send", "/child-send TEXT", "send a message to the focused child", requiredArguments},
	{"/child-cancel", "/child-cancel", "cancel the focused child", noArguments},
	{"/child-resume", "/child-resume", "confirm ownership of the focused child", noArguments},
	{"/child-retry", "/child-retry", "retry the last uncertain child request", noArguments},
	{"/children start", "/children start TEMPLATE", "start child: [PROVIDER MODEL EFFORT] -- TASK", requiredArguments},
	{"/children retry-start", "/children retry-start", "retry an uncertain child start with the same input ID", noArguments},
	{"/model", "/model [refresh]", "choose provider, model and effort; refresh reloads catalog", noArguments},
	{"/analyze", "/analyze", "read-only session analysis", noArguments},
	{"/view", "/view [MODE]", "choose Chat / Orchestration / Split (120+ columns)", noArguments},
	{"/view chat", "/view chat", "show Live Dock conversation", noArguments},
	{"/view orchestration", "/view orchestration", "show agents and Unreal-owned operations", noArguments},
	{"/view split", "/view split", "show Chat and Orchestration (120+ columns)", noArguments},
	{"/export", "/export [last | all]", "choose a public agent response to export as Markdown", noArguments},
	{"/export last", "/export last", "export the latest public agent response as Markdown", noArguments},
	{"/export all", "/export all", "export the full public conversation as Markdown", noArguments},
	{"/help", "/help", "show commands and keys", noArguments},
}

// submittedCommand runs only after an explicit submit. Paste/navigation state
// controls the command menu, not routing: registered commands never become
// public chat inputs. Only leading whitespace is removed; arguments, including
// multiline task bodies, remain intact.
func submittedCommand(input string) (string, bool) {
	line := strings.TrimLeftFunc(input, unicode.IsSpace)
	token := commandToken(line)
	if strings.HasPrefix(token, "/") {
		for _, candidate := range commandCatalog {
			if token == commandToken(candidate.Name) {
				return line, true
			}
		}
	}
	return input, false
}

func commandToken(text string) string {
	if end := strings.IndexFunc(text, unicode.IsSpace); end >= 0 {
		return text[:end]
	}
	return text
}

func candidates(s Snapshot, u UIState) []commandCandidate {
	if u.Private != nil || u.Pasted || !strings.HasPrefix(u.Input, "/") {
		return nil
	}
	if strings.HasPrefix(u.Input, "/child ") {
		prefix := strings.TrimPrefix(u.Input, "/child ")
		if strings.ContainsAny(prefix, " \n\t") {
			return nil
		}
		all := []commandCandidate{{Name: "/child " + string(s.ID), Usage: string(s.ID), Description: "(this session)"}}
		for _, r := range children(u, s) {
			all = append(all, commandCandidate{Name: "/child " + string(r.ID), Usage: string(r.ID), Description: r.Label})
		}
		var out []commandCandidate
		for _, c := range all {
			if strings.HasPrefix(c.Name, u.Input) {
				out = append(out, c)
			}
		}
		return out
	}
	if strings.ContainsAny(u.Input, " \n\t") {
		return nil
	}
	var out []commandCandidate
	for _, c := range commandCatalog {
		if strings.HasPrefix(c.Name, u.Input) {
			out = append(out, c)
		}
	}
	return out
}
func menuOptions(s Snapshot, u UIState) []commandCandidate {
	if u.Private != nil || u.Pasted || u.Sheet != nil || u.Picker != nil || !strings.HasPrefix(u.Input, "/") {
		return nil
	}
	if len(u.MenuOptions) > 0 {
		return u.MenuOptions
	}
	return candidates(s, u)
}

func selectedCommand(s Snapshot, u UIState) (commandCandidate, bool) {
	options := menuOptions(s, u)
	if len(options) == 0 {
		return commandCandidate{}, false
	}
	return options[min(max(0, u.MenuSelection), len(options)-1)], true
}

// menuWindow keeps the selection visible while preserving the previous start
// until navigation or a resize requires scrolling. Metadata never takes the
// selected row, even when only one or two rows are available.
func menuWindow(count, selected, offset, limit int) (start, end int) {
	if count == 0 || limit <= 0 {
		return 0, 0
	}
	selected = min(max(0, selected), count-1)
	if count <= limit {
		return 0, count
	}
	if limit == 1 {
		return selected, selected + 1
	}
	start = min(max(0, offset), selected)
	for {
		rows := max(1, limit-2)
		if start == 0 || start+limit-1 >= count {
			rows = limit - 1
		}
		end = min(count, start+rows)
		if selected < end {
			return start, end
		}
		start++
	}
}

func commandSheet(s Snapshot, u UIState, l layout) []line {
	rows, _ := commandSheetWithin(s, u, l, max(3, min(10, l.height/3)))
	return rows
}

func commandSheetWithin(s Snapshot, u UIState, l layout, budget int) ([]line, int) {
	if budget <= 0 || u.Private != nil || u.Pasted || u.Sheet != nil || !strings.HasPrefix(u.Input, "/") {
		return nil, 0
	}
	options := menuOptions(s, u)
	if len(options) == 0 {
		parts := strings.Fields(u.Input)
		if len(parts) > 0 {
			for _, c := range commandCatalog {
				if c.Name == parts[0] {
					return []line{l.gutter("", "", textLine(c.Usage+"  "+c.Description, meta).clip(l.text))}, 0
				}
			}
		}
		return nil, 0
	}
	limit := min(budget, max(3, min(10, l.height/3)))
	selected := min(max(0, u.MenuSelection), len(options)-1)
	start, end := menuWindow(len(options), selected, u.MenuOffset, limit)
	var out []line
	if start > 0 && limit > 1 {
		value := u.Theme.symbol("↑", "^") + " more"
		if limit == 2 && end < len(options) {
			value = u.Theme.symbol("↑↓", "^v") + " more"
		}
		out = append(out, l.gutter("", "", textLine(value, meta)))
	}
	for i := start; i < end; i++ {
		c := options[i]
		value := c.Usage
		if l.text >= 55 {
			value = fmt.Sprintf("%-30s  %s", value, c.Description)
		}
		mark, st := "", normal
		if i == selected {
			mark = u.Theme.symbol("›", "*")
			st = selectionStyle
		}
		out = append(out, l.gutter("", mark, textLine(middle(value, l.text, u.Theme), st)))
	}
	if end < len(options) && len(out) < limit {
		out = append(out, l.gutter("", "", textLine(u.Theme.symbol("↓", "v")+" more", meta)))
	}
	return out, start
}

type completion struct {
	options   []commandCandidate
	last      string
	selection int
}

func (c *completion) reset() { c.options = nil; c.last = ""; c.selection = -1 }
func (c *completion) resetMenu(u *UIState) {
	c.reset()
	u.MenuOptions = nil
	u.MenuSelection, u.MenuOffset = 0, 0
	u.MenuNavigated = false
}

func (c *completion) move(e *Editor, s Snapshot, u *UIState, delta int) bool {
	u.Input, u.Pasted = e.Text(), e.Pasted
	options := menuOptions(s, *u)
	if len(options) == 0 {
		return false
	}
	u.MenuSelection = min(max(0, min(max(0, u.MenuSelection), len(options)-1)+delta), len(options)-1)
	u.MenuNavigated = true
	return true
}

func commandPrefix(candidate commandCandidate) string {
	value := candidate.Name
	if candidate.Args != noArguments {
		value += " "
	}
	return value
}

func (c *completion) apply(e *Editor, s Snapshot, u *UIState) {
	if e.Pasted || u.Private != nil || !strings.HasPrefix(e.Text(), "/") {
		c.resetMenu(u)
		return
	}
	u.Input, u.Pasted = e.Text(), e.Pasted
	if u.MenuNavigated {
		options := menuOptions(s, *u)
		if len(options) > 0 {
			c.options = options
			c.selection = min(max(0, u.MenuSelection), len(options)-1)
			e.replace(commandPrefix(options[c.selection]))
			c.last = e.Text()
			u.MenuOptions = options
			u.MenuNavigated = false
			return
		}
	}
	if c.options != nil && e.Text() == c.last {
		c.selection = (c.selection + 1) % len(c.options)
		value := commandPrefix(c.options[c.selection])
		e.replace(value)
		c.last = value
		u.MenuOptions = c.options
		u.MenuSelection = c.selection
		return
	}
	u.Input = e.Text()
	options := candidates(s, *u)
	if len(options) == 0 {
		return
	}
	if len(options) == 1 {
		e.replace(commandPrefix(options[0]))
		c.resetMenu(u)
		return
	}
	prefix := options[0].Name
	for _, o := range options[1:] {
		g := uniseg.NewGraphemes(prefix)
		end := 0
		for g.Next() {
			_, next := g.Positions()
			if !strings.HasPrefix(o.Name, prefix[:next]) {
				break
			}
			end = next
		}
		prefix = prefix[:end]
	}
	e.replace(prefix)
	c.options = options
	c.last = prefix
	c.selection = -1
	u.MenuOptions = options
	u.MenuSelection = 0
	u.MenuOffset = 0
	u.MenuNavigated = false
}
func helpSheet(t Theme, short bool) *Sheet {
	session := []string{"session  /stop  /stop idle  /resume  /retry  /detach"}
	auth := []string{"auth     /login PROVIDER ID  /logout PROVIDER ID", "         /credentials  /methods"}
	child := []string{"child    /child ID  /child-history [after] [limit]  /child-next", "         /child-send TEXT  /child-cancel  /child-resume  /child-retry"}
	keys := []string{"keys     " + t.symbol("⏎", "enter") + " send  " + t.symbol("⇥", "tab") + " complete  " + t.symbol("↑↓", "up/down") + " menu  PgUp/PgDn scroll  ^D detach", "         ^C cancels key entry or waiting; otherwise stops the session"}
	var lines []string
	if short {
		lines = append(lines, keys...)
		lines = append(lines, child...)
		lines = append(lines, auth...)
		lines = append(lines, session...)
	} else {
		lines = append(lines, session...)
		lines = append(lines, auth...)
		lines = append(lines, child...)
		lines = append(lines, keys...)
	}
	lines = append(lines, "Type / to search commands. /model, /analyze, /export, /view. Enter/Esc closes.")
	lines = append(lines, "export   /export last  /export (choose response)  /export all (full conversation)")
	lines = append(lines, "view     /view chat  /view orchestration  /view split (120+ columns)")
	return &Sheet{Title: "help", Lines: lines}
}
