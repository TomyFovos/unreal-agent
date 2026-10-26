package permission

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// ReadOnlyCommand reports whether a shell command only reads, by the rule of
// Codex's known-safe commands: each command in it is a fixed read-only program
// without an option that writes, deletes or runs another program, and they are
// joined only by |, &&, || or ; (newlines count as ;). Such a command runs in
// every mode, as a read does. Expansions, substitutions, subshells, assignments
// and redirections other than to /dev/null or 2>&1 make it an ordinary
// command.
func ReadOnlyCommand(command string) bool {
	command, ok := expandForLoops(command)
	if !ok {
		return false
	}
	commands, ok := plainCommands(command)
	if !ok || len(commands) == 0 {
		return false
	}
	for _, words := range commands {
		if !readOnlyProgram(words) {
			return false
		}
	}
	return true
}

var forLoop = regexp.MustCompile(`for ([A-Za-z_][A-Za-z0-9_]*) in ([^;\n]*);\s*do\s+(.*?);?\s*done`)

// expandForLoops checks each `for NAME in WORDS; do BODY; done` loop by its
// runs: the words are literal, and BODY with each word in place of $NAME must
// be read-only. A loop that passes stands as `true` in the rest of the line.
func expandForLoops(command string) (string, bool) {
	ok := true
	command = forLoop.ReplaceAllStringFunc(command, func(loop string) string {
		m := forLoop.FindStringSubmatch(loop)
		lists, parsed := plainCommands(m[2])
		if !parsed || len(lists) != 1 || strings.Contains(m[3], "for ") {
			ok = false
			return loop
		}
		name := regexp.MustCompile(`\$\{?` + m[1] + `\b\}?`)
		for _, w := range lists[0] {
			if w.glob || strings.ContainsAny(w.text, "'\"$`\\ \t") || !ReadOnlyCommand(name.ReplaceAllLiteralString(m[3], w.text)) {
				ok = false
			}
		}
		return "true"
	})
	return command, ok
}

// shellWord is one word as the shell passes it on; glob marks an unquoted
// *, ? or [, which the shell may expand into other words.
type shellWord struct {
	text string
	glob bool
}

// plainCommands splits a command line into simple commands of literal words.
func plainCommands(line string) ([][]shellWord, bool) {
	var commands [][]shellWord
	var words []shellWord
	var word strings.Builder
	inWord, glob := false, false
	endWord := func() {
		if inWord {
			words = append(words, shellWord{word.String(), glob})
		}
		word.Reset()
		inWord, glob = false, false
	}
	endCommand := func() bool {
		endWord()
		if len(words) == 0 {
			return false
		}
		commands = append(commands, words)
		words = nil
		return true
	}
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == ' ' || r == '\t':
			endWord()
		case r == '\n' || r == ';':
			if !inWord && len(words) == 0 {
				if r == '\n' {
					continue // blank line
				}
				return nil, false
			}
			if !endCommand() {
				return nil, false
			}
		case r == '|' || r == '&':
			if r == '&' && (i+1 >= len(runes) || runes[i+1] != '&') {
				return nil, false // background job
			}
			if i+1 < len(runes) && runes[i+1] == r {
				i++
			} else if r == '|' && i+1 < len(runes) && runes[i+1] == '&' {
				return nil, false
			}
			if !endCommand() {
				return nil, false
			}
		case r == '>':
			// Only an output redirection to /dev/null, or 2>&1, discards
			// nothing the user keeps.
			fd := word.String()
			if inWord && fd != "1" && fd != "2" || inWord && glob {
				return nil, false
			}
			word.Reset()
			inWord, glob = false, false
			if i+1 < len(runes) && runes[i+1] == '&' {
				if i+2 >= len(runes) || fd == "" || !(fd == "2" && runes[i+2] == '1' || fd == "1" && runes[i+2] == '2') {
					return nil, false
				}
				i += 2
				if i+1 < len(runes) && !isSeparator(runes[i+1]) {
					return nil, false
				}
				continue
			}
			if i+1 < len(runes) && runes[i+1] == '>' {
				i++
			}
			for i+1 < len(runes) && (runes[i+1] == ' ' || runes[i+1] == '\t') {
				i++
			}
			target := ""
			for i+1 < len(runes) && !isSeparator(runes[i+1]) {
				i++
				target += string(runes[i])
			}
			if target != "/dev/null" {
				return nil, false
			}
		case r == '\'':
			end := slices.Index(runes[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			word.WriteString(string(runes[i+1 : i+1+end]))
			inWord = true
			i += end + 1
		case r == '"':
			end := slices.Index(runes[i+1:], '"')
			if end < 0 {
				return nil, false
			}
			text := string(runes[i+1 : i+1+end])
			if strings.ContainsAny(text, "$`\\") {
				return nil, false
			}
			word.WriteString(text)
			inWord = true
			i += end + 1
		case r == '#' && !inWord:
			return nil, false // a comment
		case r == '*' || r == '?' || r == '[' || r == ']':
			word.WriteRune(r)
			inWord, glob = true, true
		case r >= 0x80 || unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_./:@%+=,^~#", r):
			word.WriteRune(r)
			inWord = true
		default:
			return nil, false // $ ` \ ( ) { } < ! and anything else
		}
	}
	endWord()
	if len(words) > 0 {
		commands = append(commands, words)
	} else if len(commands) > 0 && !strings.HasSuffix(strings.TrimRight(line, " \t\n"), ";") {
		return nil, false // a trailing | or &&
	}
	return commands, true
}

func isSeparator(r rune) bool { return strings.ContainsRune(" \t\n;|&", r) }

// simplePrograms only read their files or standard input.
var simplePrograms = []string{
	"basename", "cat", "cd", "cmp", "cut", "df", "diff", "dirname", "du", "echo", "expr", "false", "grep",
	"head", "id", "ls", "nl", "numfmt", "paste", "printf", "pwd", "readlink", "realpath", "rev", "seq",
	"stat", "tac", "tail", "tr", "true", "uname", "wc", "which", "whoami",
}

var sedPrint = regexp.MustCompile(`^[0-9]+(,[0-9]+)?p$`)

func readOnlyProgram(words []shellWord) bool {
	name := words[0]
	if name.glob || strings.Contains(name.text, "=") {
		return false // an expanded name, or an environment assignment
	}
	args := words[1:]
	globbed := slices.ContainsFunc(args, func(w shellWord) bool { return w.glob })
	text := make([]string, len(args))
	for i, a := range args {
		text[i] = a.text
	}
	switch name.text {
	case "find":
		// A glob could expand into an option; these options write, delete
		// or run programs.
		return !globbed && !slices.ContainsFunc(text, func(a string) bool {
			return slices.Contains([]string{"-exec", "-execdir", "-ok", "-okdir", "-delete", "-fls", "-fprint", "-fprint0", "-fprintf"}, a)
		})
	case "rg":
		return !globbed && !slices.ContainsFunc(text, func(a string) bool {
			return a == "--pre" || strings.HasPrefix(a, "--pre=") || a == "--hostname-bin" || strings.HasPrefix(a, "--hostname-bin=") ||
				a == "--search-zip" || strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "z")
		})
	case "sed":
		// sed -n 5p FILE or sed -n 5,20p FILE
		return !globbed && len(text) >= 2 && len(text) <= 3 && text[0] == "-n" && sedPrint.MatchString(text[1])
	case "sort":
		return !slices.ContainsFunc(text, func(a string) bool {
			return strings.HasPrefix(a, "--output") || strings.HasPrefix(a, "--compress-program") ||
				strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "o")
		})
	case "uniq":
		// uniq IN OUT writes OUT; in a pipeline it has no file arguments.
		return !slices.ContainsFunc(text, func(a string) bool { return !strings.HasPrefix(a, "-") })
	case "git":
		return !globbed && readOnlyGit(text)
	}
	return slices.Contains(simplePrograms, name.text)
}

// readOnlyGit allows the subcommands that only read the repository. Global -c
// or --config-env options, pagers, external diff and output files can run
// programs or write files.
func readOnlyGit(args []string) bool {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		switch a := args[i]; {
		case a == "-C" && i+1 < len(args):
			i += 2
			continue
		case slices.Contains([]string{"--no-pager", "-P", "--no-optional-locks", "--literal-pathspecs", "--no-replace-objects", "--bare"}, a),
			strings.HasPrefix(a, "--git-dir="), strings.HasPrefix(a, "--work-tree="):
			i++
			continue
		}
		return false
	}
	if i == len(args) {
		return false
	}
	sub, rest := args[i], args[i+1:]
	if slices.ContainsFunc(rest, func(a string) bool {
		return slices.Contains([]string{"--output", "--ext-diff", "--textconv", "--exec", "--paginate"}, a) ||
			strings.HasPrefix(a, "--output=") || strings.HasPrefix(a, "--exec=")
	}) {
		return false
	}
	switch sub {
	case "status", "log", "diff", "show", "rev-parse", "ls-files", "ls-tree", "cat-file", "show-ref", "for-each-ref", "describe", "merge-base", "blame", "shortlog":
		return true
	case "grep":
		return !slices.ContainsFunc(rest, func(a string) bool {
			return strings.HasPrefix(a, "-O") || strings.HasPrefix(a, "--open-files-in-pager")
		})
	case "branch":
		// Without these flags, arguments create, rename or delete branches.
		return !slices.ContainsFunc(rest, func(a string) bool {
			return !slices.Contains([]string{"--list", "-l", "--show-current", "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose"}, a) && !strings.HasPrefix(a, "--format=")
		})
	case "remote":
		return len(rest) == 0 || len(rest) == 1 && (rest[0] == "-v" || rest[0] == "--verbose")
	}
	return false
}
