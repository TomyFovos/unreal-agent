package claudeagent

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// subagentCommand reports whether a Bash command only runs the Host's
// subagent command: cli, then "subagent", then literal words, with at most a
// quoted heredoc as its input. Such orchestration needs no approval: a child
// follows the Session's mode and asks its parent (U17).
func subagentCommand(command, cli string) bool {
	if cli == "" {
		return false
	}
	first, body, multiline := strings.Cut(command, "\n")
	words, delimiter, ok := literalWords(first)
	if !ok || len(words) < 2 || words[0] != cli || words[1] != "subagent" {
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
	if cli == "" || subagentCommand(command, cli) {
		return "", false
	}
	first, rest, multiline := strings.Cut(command, "\n")
	words, _, ok := literalWords(first)
	if !ok || len(words) < 2 || words[1] != "subagent" {
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
	if !subagentCommand(rewritten, cli) {
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
	return s.SettingsCommand
}

func subagentPrompt(s SubagentCLI) string {
	prompt := subagentWorkPrompt(s)
	if cli := s.SettingsCommand; cli != "" {
		if !literalCLI(cli) {
			cli = "'" + cli + "'"
		}
		prompt += fmt.Sprintf("\nOn the user's instruction, read settings with %[1]s subagent model show; set the next child's provider/model/effort with %[1]s subagent model set --role lead|worker|collector|all --provider ID --model ID [--effort LEVEL]. The Host validates and records the choice. Use API-key providers only when the user explicitly chooses them; never substitute another provider.", cli)
	}
	return prompt
}

func subagentWorkPrompt(s SubagentCLI) string {
	if len(s.RoleChoices) > 0 {
		return roleSubagentPrompt(s)
	}
	if s.Binary == "" {
		return claudeDelegationPromptFor(s)
	}
	cli := s.Binary
	if !literalCLI(cli) {
		cli = "'" + cli + "'"
	}
	m := SubagentModels{Family: "GPT", Lead: "gpt-6.2-sol", Collector: "gpt-6-luna"}
	overrides := " Add --model ID and --effort LEVEL to subagent start to override a template's model and reasoning effort."
	if s.Models != nil {
		if chosen := s.Models(); chosen.Family != "" && chosen.Lead != "" {
			// The model chosen with subagent model refuses those overrides.
			m, overrides = chosen, ""
		}
	}
	collector := "the collector template (" + m.Collector + ") and never to a " + m.Lead + " worker"
	if m.Collector == "" {
		collector = "the collector template (" + m.Lead + " at low effort) and never to a higher-effort worker"
	}
	prompt := fmt.Sprintf("\n\nYour role in this Session is to run the project: you design, decide, keep the work moving and talk with the user; %[3]s does the work, because %[3]s models cost less than you. Keep your own reasoning to design and verification planning; do not solve the task yourself, including calculations, search, algorithm trials or substantial code. Delegate that work to the lead or a worker. Review what %[3]s delivers before accepting it, especially for over-implementation: limits, checks, abstractions or features nobody asked for, and changes beyond the task. Send back what goes beyond the specification, and keep the project moving rather than redoing %[3]s's work yourself.\n"+
		"For implementation work, give your design to the implementation lead, which runs %[4]s at max effort and works through its own %[3]s workers in parallel (worker-low through worker-max). For an investigation or another small task, start one worker directly instead (worker-low or worker-medium): each %[3]s agent reads the whole AGENTS.md again, and the lead at max costs most.\n"+
		"Collector tasks, which must go to %[5]s: fetching a URL or API (curl, gh api, gh pr/issue/run view) and reporting fields; checking CI or run status and pulling the failing log lines; listing or searching files (ls, find, rg, grep) and reporting matches; reading files or logs to extract specific values, counts or sections; read-only git summaries (status, log, diff, show); disk, process and environment checks (df, du, ps); summarizing an existing report or document. Give one collector several such questions at once. A single read-only command that answers your question directly you may still run yourself; do not split such a step off.\n"+
		"Use unreal-agent subagent for parallel work and waits; Claude Code Monitor is unavailable. %[3]s children stay the default for implementation, investigation and review. Use Claude Code's Agent tool only when a Claude model is wanted for the task (for example a review the user asks to run on Fable): set its model, and it finishes within your turn. Hand the work over and end your turn; do not wait for it. While children work, the user can talk to you. When a child asks a question, asks for approval or ends, its message starts a new turn for you.\n"+
		"Turns that children's messages start run in the background: the user sees only their first line. Handle there what you can decide within the design and the permission mode: answer questions, approve or deny requests, send follow-up instructions, review results and have problems fixed. You decide; children do the work. Hand investigations, data analysis, log and file reading beyond the few files a decision needs, GitHub checks, builds and tests, writing documents and commits to the lead or a worker, and review what they report. In background turns, run yourself only commands that need no approval (reading files, read-only commands such as git diff), since your own other commands would stop the user for approval. Address the user only for what needs them, and start that message with 【報告】 (the work the user asked for is done, or failed in a way you cannot resolve) or 【要確認】 (a decision or approval beyond your authority, a change of requirements or design direction, publishing, or anything you are unsure about). When the user writes while a background turn runs, their message comes first.\n"+
		"Begin each task you give a child with a one-line title on the first line; the views show it as what the child works on.\n"+
		"Always use the command path shown below, including after an update; ignore versioned command paths in older messages. Run these commands with Bash exactly in these forms. Unreal Agent approves them itself in every permission mode, Read Only included; the children follow the mode, so in Read Only they read files and run read-only commands such as git log, git show or rg, which is enough for investigations:\n"+
		"- Start the lead, with the task in a quoted heredoc: %[1]s subagent start --template lead <<'EOF' (then the title line, the goal, design, files and interfaces, constraints and how to verify, then a line EOF)\n"+
		"- Answer a child's approval request: %[1]s subagent approve HANDLE REQUEST allow|deny. Children ask you instead of the user.\n"+
		"- Send more instructions or answers: %[1]s subagent send HANDLE <<'EOF' (text, then EOF). Stop a child: %[1]s subagent cancel HANDLE. Active children and ended counts: %[1]s subagent list; all children: %[1]s subagent list --all. Report: %[1]s subagent show HANDLE; include its original instructions with --task. Optionally record your assessment: %[1]s subagent review HANDLE accept|reject [--reason TEXT] (reason optional, for /analyze). Reuse the one-line title for rework. Do not use wait unless you know the result will arrive within tens of seconds; use at most --timeout 60s: %[1]s subagent wait HANDLE --timeout 60s.\n"+
		"Templates: %[2]s (worker-<effort> starts one worker directly).%[6]s", cli, strings.Join(s.Templates, ", "), m.Family, m.Lead, collector, overrides)
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
// A missing route is an error for that role, never a reason to use Agent or
// another provider instead.
func roleSubagentPrompt(s SubagentCLI) string {
	var p strings.Builder
	p.WriteString("\n\nRun the project: design, decide, communicate with the user, delegate work and review the results for correctness and unrequested changes. The user selected these subagent roles:\n")
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
			p.WriteString(" · unavailable; report provider_unavailable for this role and ask the user to restore its connection or change subagent model. Do not substitute another provider.\n")
		} else if role.Provider == ProviderID {
			if s.AgentModel != nil && s.AgentModel() != "" {
				fmt.Fprintf(&p, " · use Claude Code's Agent tool, which is forced to run %s; finish within this turn.\n", role.Model)
			} else {
				fmt.Fprintf(&p, " · use Claude Code's Agent tool with model set to %q for this role; finish within this turn.\n", role.Model)
			}
		} else {
			fmt.Fprintf(&p, " · Templates: %s. Start with %s subagent start --template %s <<'EOF' (title, task and verification, then EOF).", strings.Join(role.Templates, ", "), cli, role.Templates[0])
			if role.Provider == "openai-codex" {
				p.WriteString(" For this role only, --model ID and --effort LEVEL may override the template.")
			}
			p.WriteByte('\n')
		}
	}
	p.WriteString("Use lead for implementation ownership, workers for hands-on implementation, investigation and tests, and collector for URL/API retrieval, CI/log checks, file searches and extraction, read-only git summaries and document summaries. Roles may use different providers; do not assume the lead's workers use its provider or model.\n")
	if cli != "" {
		fmt.Fprintf(&p, "Optionally record your assessment with %s subagent review HANDLE accept|reject [--reason TEXT]; reason is optional. This supports /analyze. Reuse the task title for rework, and report how you verified the result.\n", cli)
		fmt.Fprintf(&p, "Use exactly %s subagent for Unreal children. Begin each task with a one-line title. Hand work over and end your turn; completion or questions start a new turn. Do not use Claude Code Monitor. Do not wait unless the result is due within tens of seconds; use at most --timeout 60s.\nAnswer child approvals yourself with %[1]s subagent approve HANDLE REQUEST allow|deny. Send instructions with %[1]s subagent send HANDLE <<'EOF' (text, then EOF). Inspect with %[1]s subagent list or %[1]s subagent show HANDLE; cancel with %[1]s subagent cancel HANDLE. Children follow the Session's permission mode and ask you for approvals.\n", cli)
	}
	return p.String()
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
