package ast

import "unicode"

// stalledQuantifier reports a query that tree-sitter v0.25's query compiler
// never finishes compiling: a + quantifier after ? or * in the same run of
// quantifiers ("_?+", "(identifier)* +", "_?*+", "__?+"). The call is native
// and cannot be canceled, so such a query is refused before it reaches
// ts_query_new. Whitespace, comments and captures may separate the
// quantifiers, including form feeds and vertical tabs accepted by tree-sitter's
// iswspace scanner; text inside strings is not a quantifier. No valid query needs
// + right after ? or *, so the check does not depend on what precedes them.
func stalledQuantifier(query string) bool {
	name := func(c rune) bool {
		return c == '_' || c == '-' || c == '.' || c == '?' || c == '!' ||
			unicode.IsLetter(c) || unicode.IsNumber(c)
	}
	chars := []rune(query)
	nullable := false // the current run of quantifiers holds ? or *
	for i := 0; i < len(chars); i++ {
		switch c := chars[i]; {
		case unicode.IsSpace(c):
		case c == ';':
			for i < len(chars) && chars[i] != '\n' {
				i++
			}
		case c == '@':
			for i+1 < len(chars) && name(chars[i+1]) {
				i++
			}
		case c == '?' || c == '*':
			nullable = true
		case c == '+':
			if nullable {
				return true
			}
		case c == '"':
			for i++; i < len(chars) && chars[i] != '"'; i++ {
				if chars[i] == '\\' {
					i++
				}
			}
			nullable = false
		default:
			nullable = false
		}
	}
	return false
}
