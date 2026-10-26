package permission

import "testing"

func TestReadOnlyCommand(t *testing.T) {
	allowed := []string{
		"git status",
		"git log --oneline -5",
		"git log -1 --format=%B",
		`git log --format="%H %an %s" -3`,
		"git show --stat HEAD",
		"git show HEAD:docs/a.md",
		"git -C /repo --no-pager diff HEAD~1",
		"git branch -a",
		"git branch",
		"git remote -v",
		"git grep -n TODO",
		"git rev-parse HEAD",
		"cd /work/repo && git log -3 && git status --short",
		"ls -la ~/.config",
		"cat a.go | grep -n func | head -20",
		"wc -l *.go",
		"grep -rn \"foo\" --include=\"*.go\" .",
		"rg -n 'func Run' harness",
		"find . -name '*.go' -type f | wc -l",
		"sed -n 1,40p main.go",
		"sort -k2 -n | uniq -c",
		"git log 2>/dev/null || true",
		"git status 2>&1 | head",
		"ls >/dev/null; pwd",
		"cat 日本語.md",
		"git status;",
		// Commands the GPT lead and worker ran on a live investigation.
		"cd /w && git show -s --format=fuller HEAD && printf '\\n---STAT---\\n' && git show --stat --oneline HEAD && git ls-files 'docs/*' 'AGENTS.md'",
		"git ls-tree -r HEAD",
		"git cat-file -p HEAD",
		"diff -u a.txt b.txt",
		// A worker read ten documents this way on devlore.
		`cd /w && for f in REQUIREMENTS.md docs/ai-instructions/開発.md; do printf '\n--- %s ---\n' "$f"; cat "$f"; done`,
		"for c in HEAD HEAD~1; do git show --stat ${c}; done",
	}
	for _, command := range allowed {
		if !ReadOnlyCommand(command) {
			t.Errorf("ReadOnlyCommand(%q) = false, want true", command)
		}
	}
	denied := []string{
		"",
		"go test ./...",
		"rm -rf x",
		"git commit -m x",
		"git push",
		"git branch -D old",
		"git -c core.pager=evil log",
		"git log --output=x",
		"git diff --ext-diff",
		"git grep -Oevil x",
		"git remote add x y",
		"git config --list",
		"echo hi > out.txt",
		"cat a >> b",
		"ls $(pwd)",
		"echo $HOME",
		"echo \"$HOME\"",
		"ls `pwd`",
		"(cd x && ls)",
		"{ ls; }",
		"ls & rm x",
		"ls |& cat",
		"ls |",
		"ls &&",
		"; ls",
		"PAGER=evil git log",
		"find . -delete",
		"find * -name x",
		"find . -exec rm {} ;",
		"rg --pre evil x",
		"rg -uz x",
		"rg x *",
		"sed -i s/a/b/ f",
		"sed -n 1p a b",
		"sort -o out in",
		"uniq in out",
		"cat <secret",
		"git log >&2",
		"git log # comment",
		"xargs rm",
		"bash -c ls",
		"/usr/bin/git status ; rm x",
		"curl https://example.com",
		"git log -1 --format=%s HEAD | awk '{ print length($0) }'",
		"for f in a b; do rm $f; done",
		"for f in *; do cat $f; done",
		"for f in -delete; do find . $f; done",
		"for f in a; do cat $g; done",
	}
	for _, command := range denied {
		if ReadOnlyCommand(command) {
			t.Errorf("ReadOnlyCommand(%q) = true, want false", command)
		}
	}
}
