package filesearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixtureFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func paths(index Index) []string { return index.files }

func TestScanIgnoreHierarchyProtectedPathsAndNoFileContentIndex(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".gitignore": "*.log\n/only-root.txt\ncache/\nbuild/*\n!build/\n!build/keep.txt\n**/generated/**\n# comment\n\\#literal\n\\!literal\nspace\\ \n",
		".ignore":    "*.temp\n", ".git/info/exclude": "local.txt\n",
		".git/config": "private", "local.txt": "excluded", "a.temp": "excluded",
		"root.log": "excluded", "only-root.txt": "excluded", "nested/only-root.txt": "kept",
		"nested/.gitignore": "!keep.log\n/nested-only.txt\n",
		"nested/keep.log":   "kept", "nested/lost.log": "excluded", "nested/nested-only.txt": "excluded",
		"cache/keep.txt": "excluded", "cache/.gitignore": "!keep.txt\n",
		"build/keep.txt": "kept", "build/drop.txt": "excluded", "nested/generated/deep/a.go": "excluded",
		"#literal": "excluded", "!literal": "excluded", "space ": "excluded",
		"docs/猫 👩🏽‍💻é.md": "CONTENT_NEVER_INDEXED", "src/main.go": "CONTENT_NEVER_INDEXED",
		".env": "SECRET", ".env.example": "SECRET", "nested/.env.production": "SECRET",
		"auth.json": "SECRET", ".ssh/id_rsa": "SECRET", ".claude/auth": "SECRET", "key.pem": "SECRET",
		"credentials.toml": "SECRET", ".git": "", // created below as a directory
	}
	delete(files, ".git")
	for name, content := range files {
		fixtureFile(t, root, name, content)
	}
	result, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("scan: %+v %v", result, err)
	}
	var visible []string
	for _, name := range result.Index.files {
		if name == ".ignore" || name == ".gitignore" || name == "nested/.gitignore" {
			continue
		}
		visible = append(visible, name)
	}
	want := []string{"build/keep.txt", "docs/猫 👩🏽‍💻é.md", "nested/keep.log", "nested/only-root.txt", "src/main.go"}
	if !reflect.DeepEqual(visible, want) {
		t.Fatalf("visible %q want %q", visible, want)
	}
	matches, err := result.Index.Search(context.Background(), "CONTENT_NEVER_INDEXED", 10)
	if err != nil || len(matches) != 0 {
		t.Fatal("file contents reached index", matches, err)
	}
}

func TestScanIgnoreMetadataFailsClosedAndWorktreeGitFile(t *testing.T) {
	for _, scenario := range []string{"malformed", "symlink", "nested"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			fixtureFile(t, root, "hidden.txt", "private")
			switch scenario {
			case "malformed":
				fixtureFile(t, root, ".gitignore", "[broken\n")
			case "symlink":
				outside := t.TempDir()
				fixtureFile(t, outside, "ignore", "hidden.txt")
				if err := os.Symlink(filepath.Join(outside, "ignore"), filepath.Join(root, ".gitignore")); err != nil {
					t.Fatal(err)
				}
			case "nested":
				fixtureFile(t, root, "nested/.gitignore", "[broken")
				fixtureFile(t, root, "nested/secret.txt", "private")
			}
			result, err := Scan(context.Background(), root)
			if err == nil || !strings.Contains(err.Error(), ".gitignore") || len(result.Index.files) != 0 {
				t.Fatalf("ignore failure needs its filename and no partial success: %+v %v", result, err)
			}
		})
	}
	root := t.TempDir()
	fixtureFile(t, root, ".git", "gitdir: /private/other/worktree")
	fixtureFile(t, root, "kept", "data")
	result, err := Scan(context.Background(), root)
	if err != nil || !reflect.DeepEqual(paths(result.Index), []string{"kept"}) {
		t.Fatal(result, err)
	}
}

func TestScanKeepsDeepFilesAndLargeIgnoreRules(t *testing.T) {
	root := t.TempDir()
	deep := strings.Repeat("d/", 40) + "kept"
	fixtureFile(t, root, deep, "data")
	fixtureFile(t, root, ".gitignore", strings.Repeat("missing\n", 10000)+"../outside\n")
	r, err := Scan(t.Context(), root)
	if err != nil || !reflect.DeepEqual(r.Index.files, []string{".gitignore", deep}) {
		t.Fatal(r, err)
	}
}

func TestScanAndSearchCancellationDiscardPartialResults(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "one", "data")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := Scan(ctx, root)
	if !errors.Is(err, context.Canceled) || len(r.Index.files) != 0 {
		t.Fatal(r, err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r, err = Scan(ctx, root)
	if !errors.Is(err, context.DeadlineExceeded) || len(r.Index.files) != 0 {
		t.Fatal(r, err)
	}
	index := Index{files: []string{"one"}}
	matches, err := index.Search(ctx, "", 32)
	if !errors.Is(err, context.DeadlineExceeded) || len(matches) != 0 {
		t.Fatal(matches, err)
	}
}

func TestSafeNamesRejectTerminalControlsAndBidiPreserveGraphemes(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"猫👩🏽‍💻é.md", "space file", "quote\"file", "back\\slash", "unsafe\x1b[31m", "unsafe\nfile", "unsafe\u202efile"} {
		fixtureFile(t, root, name, "data")
	}
	r, err := Scan(context.Background(), root)
	if err != nil || len(r.Index.files) != 4 {
		t.Fatal(r, err)
	}
	for _, name := range r.Index.files {
		if !SafePath(name) {
			t.Fatal(name)
		}
	}
}

func TestSafePathRejectsInvalidUTF8WithoutFilesystemSupport(t *testing.T) {
	// APFS refuses these names before Scan can see them. The pure path boundary
	// must still reject invalid bytes on every OS, including when no file exists.
	for _, name := range []string{string([]byte{0xff}), "dir/" + string([]byte{0xc3, 0x28}), "file" + string([]byte{0xe2, 0x82})} {
		if SafePath(name) {
			t.Fatal("invalid UTF-8 accepted as a file candidate")
		}
	}
}

func TestIgnoreGlobsEscapesAnchorsAndNegation(t *testing.T) {
	for _, test := range []struct {
		pattern, name string
		dir, want     bool
	}{
		{"foo", "a/foo", false, true}, {"/foo", "a/foo", false, false},
		{"a/**/b", "a/b", false, true}, {"a/**/b", "a/x/y/b", false, true},
		{"**/foo", "foo", false, true}, {"**/foo", "a/foo", false, true},
		{"*.log\n!keep.log", "x/keep.log", false, false},
		{"dir/", "dir", true, true}, {"dir/", "dir", false, false},
		{"f[0-9]?.go", "f32.go", false, true}, {"f[0-9]?.go", "fxx.go", false, false},
		{"\\#file", "#file", false, true}, {"\\!file", "!file", false, true},
		{"space\\ ", "space ", false, true}, {"space  ", "space", false, true},
	} {
		rules, err := parseRules(test.pattern, ".")
		if err != nil || ignored(rules, test.name, test.dir) != test.want {
			t.Fatalf("%q %q: %v", test.pattern, test.name, err)
		}
	}
}

func TestScanExcludesSymlinksAndOutsideFiles(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	fixtureFile(t, root, "real.go", "source")
	fixtureFile(t, outside, "outside.go", "private")
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "outside.go"), filepath.Join(root, "linked.go")); err != nil {
		t.Fatal(err)
	}
	result, err := Scan(t.Context(), root)
	if err != nil || !reflect.DeepEqual(paths(result.Index), []string{"real.go"}) {
		t.Fatal(result, err)
	}
}
