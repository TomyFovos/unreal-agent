package ast

import "testing"

// Cover the stalled native queries and their whitespace/capture variants.
// Other repetitions, separate patterns, strings and comments remain allowed.
func TestStalledQuantifier(t *testing.T) {
	for _, query := range []string{
		"_?+0", "_?+", "_*+", "_?*+", "_*?+", "_+*+", "_?++", "_? +", "_?\n+",
		"_?; comment\n+", "(identifier)?+", "(identifier)* +", "(_)?+",
		"((identifier) @a)?+", "[(identifier)]?+", "(identifier)? @x +",
		"(identifier)?+ @x", "(identifier) @x ?+", `"("?+`, `"function"?+`, "_?_?+0", "(a)? _? +", "__?+",
		"_?_?\f+0", "_?\v+", "_*\f+", "_?_?\v*\f+", "_?\f; comment\n\v+",
		"(identifier)?\f@match!\v+", "(identifier)?\u2003+",
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
		"(identifier)\f+ (identifier)\v?", "_?\f_+", "_?\v(identifier)+",
		`((identifier) @match? (#match? @match? "x?+"))`, `(identifier) @match? +`,
		`(identifier) @match! +`, `"escaped \" ?+"`, "(identifier) ; ?\f+\n@match",
	} {
		if stalledQuantifier(query) {
			t.Errorf("%q was refused", query)
		}
	}
}

func TestStalledQuantifierSeparators(t *testing.T) {
	for _, prefix := range []string{"_?", "_*", "_?_?", "(identifier)? @capture!"} {
		for _, separator := range []string{" ", "\t", "\n", "\r", "\f", "\v", "\r\n\f\v", "; comment\n\f"} {
			query := prefix + separator + "+0"
			if !stalledQuantifier(query) {
				t.Errorf("%q bypassed the quantifier guard", query)
			}
		}
	}
}
