package ast

// stalledQuantifier reports a query that tree-sitter v0.25's query compiler
// never finishes compiling: a + quantifier after ? or * in the same run of
// quantifiers ("_?+", "(identifier)* +", "_?*+", "__?+"). The call is native
// and cannot be canceled, so such a query is refused before it reaches
// ts_query_new. Whitespace, comments and captures may separate the
// quantifiers; text inside strings is not a quantifier. No valid query needs
// + right after ? or *, so the check does not depend on what precedes them.
func stalledQuantifier(query string) bool {
	name := func(c byte) bool {
		return c == '_' || c == '-' || c == '.' ||
			'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
	}
	nullable := false // the current run of quantifiers holds ? or *
	for i := 0; i < len(query); i++ {
		switch c := query[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		case c == ';':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case c == '@':
			for i+1 < len(query) && name(query[i+1]) {
				i++
			}
		case c == '?' || c == '*':
			nullable = true
		case c == '+':
			if nullable {
				return true
			}
		case c == '"':
			for i++; i < len(query) && query[i] != '"'; i++ {
				if query[i] == '\\' {
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
