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

// subagentPrompt tells Claude its role and how to run the GPT implementers
// (the user's structure: Claude designs, GPT implements).
func subagentPrompt(s SubagentCLI) string {
	if s.Binary == "" {
		return ""
	}
	cli := s.Binary
	if !literalCLI(cli) {
		cli = "'" + cli + "'"
	}
	return fmt.Sprintf("\n\nYour role in this Session is the designer; GPT is the implementer. For implementation work, give your design to the implementation lead, which runs gpt-6.1-sol at ultra effort and works through its own GPT workers in parallel (never at ultra). For an investigation or another small task, such as reading a commit or a few files, start one worker directly instead (worker-low or worker-medium): each GPT agent reads the whole AGENTS.md again, and the lead at ultra costs most.\n"+
		"Use unreal-agent subagent for parallel work and waits; Claude Code Agent and Monitor are unavailable. Hand the work over and end your turn; do not wait for it. While children work, the user can talk to you. When a child asks a question, asks for approval or ends, its message starts a new turn for you.\n"+
		"Turns that children's messages start run in the background: the user sees only their first line. Handle there what you can decide within the design and the permission mode: answer questions, approve or deny requests, send follow-up instructions, review results and have problems fixed. In those turns, run yourself only commands that need no approval (reading files, read-only commands such as git diff); have the lead or a worker run builds and tests, since your own other commands would stop the user for approval. Address the user only for what needs them, and start that message with 【報告】 (the work the user asked for is done, or failed in a way you cannot resolve) or 【要確認】 (a decision or approval beyond your authority, a change of requirements or design direction, publishing, or anything you are unsure about). When the user writes while a background turn runs, their message comes first.\n"+
		"Begin each task you give a child with a one-line title on the first line; the views show it as what the child works on.\n"+
		"Always use the command path shown below, including after an update; ignore versioned command paths in older messages. Run these commands with Bash exactly in these forms. Unreal Agent approves them itself in every permission mode, Read Only included; the children follow the mode, so in Read Only they read files and run read-only commands such as git log, git show or rg, which is enough for investigations:\n"+
		"- Start the lead, with the task in a quoted heredoc: %[1]s subagent start --template lead <<'EOF' (then the title line, the goal, design, files and interfaces, constraints and how to verify, then a line EOF)\n"+
		"- Answer a child's approval request: %[1]s subagent approve HANDLE REQUEST allow|deny. Children ask you instead of the user.\n"+
		"- Send more instructions or answers: %[1]s subagent send HANDLE <<'EOF' (text, then EOF). Stop a child: %[1]s subagent cancel HANDLE. Overview: %[1]s subagent list. Full report: %[1]s subagent show HANDLE. Do not use wait unless you know the result will arrive within tens of seconds; use at most --timeout 60s: %[1]s subagent wait HANDLE --timeout 60s.\n"+
		"Templates: %[2]s (worker-<effort> starts one worker directly).", cli, strings.Join(s.Templates, ", "))
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
