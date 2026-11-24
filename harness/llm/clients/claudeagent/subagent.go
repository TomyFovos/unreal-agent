package claudeagent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// subagentCommand reports whether a Bash command only runs the Host's
// subagent command: cli, then "subagent", then literal words, with at most a
// quoted heredoc as its input. Such orchestration needs no approval: a child
// follows the Session's mode and asks its parent (U17).
func subagentCommand(command, cli string) bool {
	return literalSessionCommand(command, cli, "subagent")
}

func literalSessionCommand(command, cli string, groups ...string) bool {
	if cli == "" {
		return false
	}
	first, body, multiline := strings.Cut(command, "\n")
	words, delimiter, ok := literalWords(first)
	if !ok || len(words) < 2 || words[0] != cli || !slices.Contains(groups, words[1]) {
		return false
	}
	if delimiter == "" {
		return !multiline || strings.TrimSpace(body) == ""
	}
	if !multiline {
		return false
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if line == delimiter {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n")) == ""
		}
	}
	return false
}

// currentSubagentCommand rewrites a subagent command that names another
// Unreal executable (a versioned path Claude kept from before an update, or
// the bare name) to this Host's command. Running the old binary would reach
// an old Host, and an unrecognized path would stop the user for approval.
func currentSubagentCommand(command, cli string) (string, bool) {
	return currentSessionCommand(command, cli, "subagent")
}

func currentSessionCommand(command, cli string, groups ...string) (string, bool) {
	if cli == "" || literalSessionCommand(command, cli, groups...) {
		return "", false
	}
	first, rest, multiline := strings.Cut(command, "\n")
	words, _, ok := literalWords(first)
	if !ok || len(words) < 2 || !slices.Contains(groups, words[1]) {
		return "", false
	}
	if name := filepath.Base(words[0]); name != "unreal" && name != "unreal-agent" {
		return "", false
	}
	line := strings.TrimLeft(first, " \t")
	end := strings.IndexAny(line, " \t")
	if line[0] == '\'' || line[0] == '"' {
		end = strings.IndexByte(line[1:], line[0]) + 2
	}
	if end <= 0 || end > len(line) {
		return "", false
	}
	token := cli
	if !literalCLI(cli) {
		token = "'" + cli + "'"
	}
	rewritten := token + line[end:]
	if multiline {
		rewritten += "\n" + rest
	}
	if !literalSessionCommand(rewritten, cli, groups...) {
		return "", false
	}
	return rewritten, true
}

// literalWords splits one shell line into words that expand to themselves:
// plain characters, single-quoted text, and double-quoted text without $, `
// or \. A final <<'TAG' or <<"TAG" names a quoted heredoc's delimiter.
// Anything else (operators, expansions, redirections) is not literal.
func literalWords(line string) (words []string, delimiter string, ok bool) {
	var word strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(line); {
		ch := line[i]
		switch {
		case ch == ' ' || ch == '\t':
			flush()
			i++
		case ch == '\'' || ch == '"':
			end := strings.IndexByte(line[i+1:], ch)
			if end < 0 {
				return nil, "", false
			}
			text := line[i+1 : i+1+end]
			if ch == '"' && strings.ContainsAny(text, "$`\\") {
				return nil, "", false
			}
			word.WriteString(text)
			inWord = true
			i += end + 2
		case strings.HasPrefix(line[i:], "<<") && !inWord:
			flush()
			tag := strings.TrimRight(line[i+2:], " \t")
			if len(tag) < 3 || tag[0] != '\'' && tag[0] != '"' || tag[len(tag)-1] != tag[0] {
				return nil, "", false
			}
			tag = tag[1 : len(tag)-1]
			if tag == "" || strings.Trim(tag, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_") != "" {
				return nil, "", false
			}
			return words, tag, true
		case 'a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || '0' <= ch && ch <= '9' || strings.IndexByte("_./:=@%+,-", ch) >= 0:
			word.WriteByte(ch)
			inWord = true
			i++
		default:
			return nil, "", false
		}
	}
	flush()
	return words, "", true
}

// subagentCommandPath keeps the settings CLI reachable without child templates.
func subagentCommandPath(s SubagentCLI) string {
	if s.Binary != "" {
		return s.Binary
	}
	if s.ChildCommand != "" {
		return s.ChildCommand
	}
	return s.SettingsCommand
}

func sessionCommandAvailable(s SubagentCLI) bool {
	return subagentCommandPath(s) != "" || s.ContextCommand != ""
}

func subagentPrompt(s SubagentCLI) string {
	prompt := subagentWorkPrompt(s)
	if cli := s.ChildCommand; cli != "" {
		if !literalCLI(cli) {
			cli = "'" + cli + "'"
		}
		prompt += fmt.Sprintf("\n\nYou own the task from your parent. To use SendParent, run %s subagent send-parent --text 'MESSAGE' in Bash; add --progress for an update that should not wake the parent. End your turn after asking a question or while waiting for children. When all work is done, your final reply is your Finish report. Include changed files, tests and blockers in the reply. If the task failed, start the reply with the line Status: failed.", cli)
	}
	if cli := s.SettingsCommand; cli != "" {
		if !literalCLI(cli) {
			cli = "'" + cli + "'"
		}
		prompt += fmt.Sprintf("\nOn the user's instruction, read this Session's role settings with %[1]s subagent model show. Set them with %[1]s subagent model set --role lead|worker|collector|all --provider ID --model ID [--effort LEVEL]. The Host validates and records the choice. Use API-key providers only when the user explicitly chooses them; never substitute another provider.", cli)
	}
	if cli := s.ContextCommand; cli != "" {
		if !literalCLI(cli) {
			cli = "'" + cli + "'"
		}
		prompt += fmt.Sprintf("\nUse these Session commands through literal Bash in every permission mode. Read current state with %[1]s state show. Record decisions with %[1]s state decide 'TEXT' [--why 'TEXT'] and steps with %[1]s state plan ID --text 'TEXT' --status todo|doing|done|dropped. Quote a user's explicit rule with %[1]s state rule add 'EXCERPT' --source LOCATOR. Record child reviews with %[1]s state accept HANDLE or %[1]s state reject HANDLE [--why 'TEXT']; these use the same review as subagent review. Use %[1]s state question 'TEXT' to note a question and %[1]s state answer ID --source LOCATOR to link the user message that answers it. Questions beginning with 【要確認】 in your reply are recorded automatically. Find raw evidence with %[1]s record find 'QUERY' and read it with %[1]s record show 'LOCATOR'. Use --help after a command for its flags.", cli)
	}
	return prompt
}

func subagentWorkPrompt(s SubagentCLI) string {
	if len(s.RoleChoices) > 0 {
		return roleSubagentPrompt(s)
	}
	if s.ChildCommand != "" {
		if s.Binary == "" {
			return ""
		}
		return fmt.Sprintf("\n\nDelegate with Unreal's SubagentStart through Bash. Templates: %s.\n", strings.Join(s.Templates, ", ")) + roleSubagentCommands(s.Binary)
	}
	if s.Binary == "" {
		return "\n\nUse Unreal's SubagentStart for all delegation. If the requested role is unavailable, report the connection problem. Check /login, then run /model refresh after signing in."
	}
	cli := s.Binary
	if !literalCLI(cli) {
		cli = "'" + cli + "'"
	}
	m := SubagentModels{Family: "GPT", Lead: "gpt-6.2-sol", Collector: "gpt-6-luna"}
	overrides := " Add --model ID to choose a model for this start, --provider ID to select its connection when needed, and --effort LEVEL to set its reasoning effort."
	if s.Models != nil {
		if chosen := s.Models(); chosen.Family != "" && chosen.Lead != "" {
			m = chosen
		}
	}
	collector := "the collector template (" + m.Collector + ") and never to a " + m.Lead + " worker"
	if m.Collector == "" {
		collector = "the collector template (" + m.Lead + " at low effort) and never to a higher-effort worker"
	}
	prompt := fmt.Sprintf("\n\nYour role in this Session is to run the project: you design, decide, keep the work moving and talk with the user; %[3]s does the work, because %[3]s models cost less than you. Keep your own reasoning to design and verification planning; do not solve the task yourself, including calculations, search, algorithm trials or substantial code. Delegate that work to the lead or a worker. Review what %[3]s delivers before accepting it, especially for over-implementation: limits, checks, abstractions or features nobody asked for, and changes beyond the task. Send back what goes beyond the specification, and keep the project moving rather than redoing %[3]s's work yourself.\n"+
		"For implementation work, give your design to the implementation lead, which runs %[4]s at max effort and works through its own %[3]s workers in parallel (worker-low through worker-max). Each %[3]s agent reads the whole AGENTS.md again. Worker and collector templates started directly by the main Session run on the lead model and effort. Send hands-on work through a lead, which delegates to workers and collectors and reviews their results before reporting to you.\n"+
		"Collector tasks, which must go to %[5]s: fetching a URL or API (curl, gh api, gh pr/issue/run view) and reporting fields; checking CI or run status and pulling the failing log lines; listing or searching files (ls, find, rg, grep) and reporting matches; reading files or logs to extract specific values, counts or sections; read-only git summaries (status, log, diff, show); disk, process and environment checks (df, du, ps); summarizing an existing report or document. Give one collector several such questions at once. A single read-only command that answers your question directly you may still run yourself; do not split such a step off.\n"+
		"Use unreal-agent subagent for all delegation and waits. Claude Code Monitor is unavailable. To delegate to another model, use the selected Unreal role or set its connection and model on the user's instruction. Continue independent work while children run. Their questions, approval requests and reports arrive at your next tool-batch boundary. If you are idle, their messages start a new turn. While children work, the user can talk to you.\n"+
		"Turns that children's messages start run in the background: the user sees only their first line. Handle there what you can decide within the design and the permission mode: answer questions, approve or deny requests, send follow-up instructions, review results and have problems fixed. You decide; children do the work. Hand investigations, data analysis, log and file reading beyond the few files a decision needs, GitHub checks, builds and tests, writing documents and commits to the lead or a worker, and review what they report. In background turns, run yourself only commands that need no approval (reading files, read-only commands such as git diff), since your own other commands would stop the user for approval. Address the user only for what needs them, and start that message with 【報告】 (the work the user asked for is done, or failed in a way you cannot resolve) or 【要確認】 (a decision or approval beyond your authority, a change of requirements or design direction, publishing, or anything you are unsure about). When the user writes while a background turn runs, their message comes first.\n"+
		"Set --title to a one-line title for the child view, or put the title on the first line of the task text.\n"+
		"Always use the command path shown below, including after an update; ignore versioned command paths in older messages. Run these commands with Bash exactly in these forms. Unreal Agent approves them itself in every permission mode, Read Only included; the children follow the mode, so in Read Only they read files and run read-only commands such as git log, git show or rg, which is enough for investigations:\n"+
		"- Start the lead with %[1]s subagent start --template lead <<'EOF' (task instructions, then EOF). The command accepts optional --title, --purpose, --inputs, --outputs, --verification and --acceptance-criteria, and --task for free text. The harness fills the launch conditions and report guidance and prints the brief the child receives.\n"+
		"- Answer a child's approval request: %[1]s subagent approve HANDLE REQUEST allow|deny. Children ask you instead of the user.\n"+
		"- Send more instructions or answers: %[1]s subagent send HANDLE <<'EOF' (text, then EOF). Stop a child: %[1]s subagent cancel HANDLE. Active children and ended counts: %[1]s subagent list; all children: %[1]s subagent list --all. The ended notice includes the completion report. Use %[1]s subagent show HANDLE to reread it, with --task for the brief. Optionally record your assessment: %[1]s subagent review HANDLE accept|reject [--reason TEXT] (reason optional, for /analyze). Reuse the one-line title for rework. Do not use wait unless you know the result will arrive within tens of seconds; use at most --timeout 60s: %[1]s subagent wait HANDLE --timeout 60s.\n"+
		"Templates: %[2]s.%[6]s", cli, strings.Join(s.Templates, ", "), m.Family, m.Lead, collector, overrides)
	if m.Family == "GPT" {
		if s.LeadModel != "" {
			prompt = strings.ReplaceAll(prompt, "gpt-6.2-sol", s.LeadModel)
		}
		if s.LeadEffort != "" {
			prompt = strings.ReplaceAll(prompt, "at max effort", "at "+s.LeadEffort+" effort")
		}
	}
	return prompt
}

// Role guidance names only the templates available on each selected provider.
// A missing route is an error for that role.
func roleSubagentPrompt(s SubagentCLI) string {
	var p strings.Builder
	if s.ChildCommand == "" {
		p.WriteString("\n\nRun the project: design, decide, communicate with the user, delegate work and review the results for correctness and unrequested changes.")
	}
	p.WriteString("\n\nUse Unreal's SubagentStart for these roles:\n")
	cli := s.Binary
	if cli != "" && !literalCLI(cli) {
		cli = "'" + cli + "'"
	}
	for _, role := range s.RoleChoices {
		fmt.Fprintf(&p, "- %s: %s · %s", role.Role, role.Provider, role.Model)
		if role.Effort != "" {
			fmt.Fprintf(&p, " · %s effort", role.Effort)
		}
		if !role.Available {
			p.WriteString(" · unavailable; report provider_unavailable for this role. Use /login for recovery and /model refresh after signing in. Change providers only on the user's instruction.\n")
		} else if s.ChildCommand == "" && role.Role != "lead" {
			fmt.Fprintf(&p, " · Templates: %s. Use these through a lead.\n", strings.Join(role.Templates, ", "))
		} else {
			fmt.Fprintf(&p, " · Templates: %s. Start with %s subagent start --template %s <<'EOF' (task instructions, then EOF).", strings.Join(role.Templates, ", "), cli, role.Templates[0])
			if role.Provider == "openai-codex" {
				p.WriteString(" For this role only, --model ID and --effort LEVEL may override the template.")
			}
			p.WriteByte('\n')
		}
	}
	p.WriteString("Use lead for implementation ownership, workers for hands-on implementation, investigation and tests, and collector for URL/API retrieval, CI/log checks, file searches and extraction, read-only git summaries and document summaries. Roles may use different providers; do not assume the lead's workers use its provider or model.\n")
	if s.ChildCommand == "" {
		p.WriteString("Worker and collector templates started directly by the main Session run on the lead model and effort. Worker and collector role selections apply only below a lead, including deeper descendants. Send hands-on work through a lead, which delegates and reviews results before reporting to you.\n")
	}
	if cli != "" {
		p.WriteString("For Unreal children, start accepts optional --title, --purpose, --inputs, --outputs, --verification and --acceptance-criteria. Task instructions can go in --task or a quoted heredoc. The command prints the brief the child receives. The ended notice includes the completion report.\n")
		fmt.Fprintf(&p, "Optionally record your assessment with %s subagent review HANDLE accept|reject [--reason TEXT]; reason is optional. This supports /analyze. Reuse the task title for rework, and report how you verified the result.\n", cli)
		p.WriteString(roleSubagentCommands(s.Binary))
	}
	return p.String()
}

func roleSubagentCommands(cli string) string {
	if !literalCLI(cli) {
		cli = "'" + cli + "'"
	}
	return fmt.Sprintf("Use exactly %s subagent for Unreal children. Start with %[1]s subagent start --template NAME <<'EOF' (task instructions, then EOF). Use --model ID to choose a model for this start, --provider ID when the model has multiple connections, and --effort LEVEL to set its reasoning effort. Begin each task with a one-line title. Continue independent work while children run. Their questions, approval requests and reports arrive at your next tool-batch boundary or wake you when idle. Claude Code Monitor is unavailable. Do not wait unless the result is due within tens of seconds. Use at most --timeout 60s.\nAnswer child approvals with %[1]s subagent approve HANDLE REQUEST allow|deny. Send instructions with %[1]s subagent send HANDLE <<'EOF' (text, then EOF). Inspect with %[1]s subagent list or %[1]s subagent show HANDLE. Cancel with %[1]s subagent cancel HANDLE. Children follow the Session's permission mode and ask you for approvals.\n", cli)
}

func literalCLI(path string) bool {
	words, delimiter, ok := literalWords(path)
	return ok && delimiter == "" && len(words) == 1 && words[0] == path
}

// This is only an interruption hint, never an approval decision. Expansions,
// wrappers and pipes must not hide a wait. subagentCommand stays strict.
var subagentWaitWords = regexp.MustCompile(`\bsubagent['"]?[\t\r\n ]+['"]?wait\b`)

func subagentWait(command string) bool {
	return subagentWaitWords.MatchString(command)
}
