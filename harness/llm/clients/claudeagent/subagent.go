package claudeagent

import (
	"fmt"
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
	return fmt.Sprintf("\n\nYour role in this Session is the designer; GPT is the implementer. For implementation work, give your design to the implementation lead, which runs gpt-6.1-sol at ultra effort and starts its own GPT workers (never at ultra), and review what it reports before you answer. For an investigation or another small task, such as reading a commit or a few files, start one worker directly instead (worker-low or worker-medium): each GPT agent reads the whole AGENTS.md again, and the lead at ultra costs most. "+
		"Run these commands with Bash exactly in these forms. Unreal Agent approves them itself in every permission mode, Read Only included; the children follow the mode, so in Read Only they read files and run read-only commands such as git log, git show or rg, which is enough for investigations:\n"+
		"- Start the lead, with the task in a quoted heredoc: %[1]s subagent start --template lead <<'EOF' (then the goal, design, files and interfaces, constraints and how to verify, then a line EOF)\n"+
		"- Wait until a child finishes or asks for approval, with the handle start printed (at most 9 minutes, so give that Bash call a 600000 ms timeout; run it again while the child runs): %[1]s subagent wait HANDLE\n"+
		"- Answer a child's approval request: %[1]s subagent approve HANDLE REQUEST allow|deny. When the Session asks for approval, children ask you instead of the user; ask the user in your reply when you are unsure.\n"+
		"- Send more instructions: %[1]s subagent send HANDLE <<'EOF' (text, then EOF). Stop a child: %[1]s subagent cancel HANDLE. Overview: %[1]s subagent list. Full report: %[1]s subagent show HANDLE.\n"+
		"Templates: %[2]s (worker-<effort> starts one worker directly).", cli, strings.Join(s.Templates, ", "))
}

func literalCLI(path string) bool {
	words, delimiter, ok := literalWords(path)
	return ok && delimiter == "" && len(words) == 1 && words[0] == path
}
