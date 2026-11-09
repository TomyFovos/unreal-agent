package ast

import "testing"

// The stalled cases were measured against tree-sitter v0.25's ts_query_new:
// each never returned. The accepted ones returned (as a query or an error).
func TestStalledQuantifier(t *testing.T) {
	for _, query := range []string{
		"_?+0", "_?+", "_*+", "_?*+", "_*?+", "_+*+", "_?++", "_? +", "_?\n+",
		"_?; comment\n+", "(identifier)?+", "(identifier)* +", "(_)?+",
		"((identifier) @a)?+", "[(identifier)]?+", "(identifier)? @x +",
		"(identifier)?+ @x", "(identifier) @x ?+", `"("?+`, `"function"?+`, "_?_?+0", "(a)? _? +", "__?+",
	} {
		if !stalledQuantifier(query) {
			t.Errorf("%q compiles forever but was accepted", query)
		}
	}
	for _, query := range []string{
		"_+?", "_??", "_?*", "_++", "_?", "_+", "(identifier)+?", "(identifier)??",
		"(identifier)**", `(call_expression) @match`,
		`((identifier) @match (#eq? @match "a+"))`, `((identifier) @m? (#match? @m "x?+"))`,
		`(identifier) @match ; a?+ in a comment`, `(string "?+") @match`,
		`(a_b)+ (c)?`, "(identifier)+ (identifier)?",
	} {
		if stalledQuantifier(query) {
			t.Errorf("%q was refused", query)
		}
	}
}
