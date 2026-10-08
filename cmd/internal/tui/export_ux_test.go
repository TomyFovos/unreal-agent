package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/analysis"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func exportFiles(t *testing.T, dir string, count int) []string {
	t.Helper()
	var paths []string
	waitFor(t, func() bool {
		files, err := os.ReadDir(dir)
		if err != nil || len(files) != count {
			return false
		}
		paths = nil
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".md") || strings.HasPrefix(file.Name(), ".") {
				return false
			}
			paths = append(paths, filepath.Join(dir, file.Name()))
		}
		return true
	})
	return paths
}

func TestLiveExportLastPickerAndAllNoCanonicalMutationOrScrollReset(t *testing.T) {
	text := "EXPORT FIRST\n" + strings.Repeat("export canonical line\n", 300) + "EXPORT LAST"
	f := conversationFixture("old agent response", text)
	keys := make(chan Key, 16)
	out := &screenObserver{}
	done := make(chan error, 1)
	dir := filepath.Join(t.TempDir(), "exports")
	theme := Theme{Plain: true}
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme, ExportDirectory: dir})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "EXPORT LAST") })
	keys <- Key{Name: "pageup"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "scrollback:") })
	keys <- Key{Text: "/export last"}
	keys <- Key{Name: "enter"}
	paths := exportFiles(t, dir, 1)
	waitFor(t, func() bool { return strings.Contains(out.text(), "exported to") })
	if exportBody(t, paths[0]) != text || !strings.Contains(out.text(), "scrollback:") {
		t.Fatal("export last used clipped text or reset scroll")
	}
	keys <- Key{Text: "/export"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Export response") })
	keys <- Key{Name: "down"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "› Turn 1") })
	keys <- Key{Name: "enter"}
	paths = exportFiles(t, dir, 2)
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "exported to") && !strings.Contains(out.text(), "cancel waiting")
	})
	var selected bool
	for _, path := range paths {
		selected = selected || exportBody(t, path) == "old agent response"
	}
	if !selected || !strings.Contains(out.text(), "scrollback:") {
		t.Fatal("picker did not export correct canonical response or changed scroll")
	}
	// Esc closes the conversation picker without exporting or opening analysis.
	keys <- Key{Text: "/export"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "Export response") })
	keys <- Key{Name: "escape"}
	waitFor(t, func() bool { return !strings.Contains(out.text(), "Export response") })
	if files, _ := os.ReadDir(dir); len(files) != 2 {
		t.Fatal("Esc exported a response")
	}
	keys <- Key{Text: "/export all"}
	keys <- Key{Name: "enter"}
	paths = exportFiles(t, dir, 3)
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "exported to") && !strings.Contains(out.text(), "cancel waiting")
	})
	var whole bool
	for _, path := range paths {
		if strings.HasPrefix(filepath.Base(path), "session-") {
			body := exportBody(t, path)
			whole = strings.Contains(body, "old agent response") && strings.Contains(body, text) && strings.Count(body, "### You\n") == 2
		}
	}
	if !whole || !strings.Contains(out.text(), "scrollback:") {
		t.Fatal("all export missing public conversation or changed transcript scroll")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 0 || len(f.view.Operations) != 0 {
		t.Fatal("export submitted a canonical input/operation")
	}
}

type gatedExportSubmit struct {
	*pagedTranscriptClient
	release <-chan struct{}
}

func (f *gatedExportSubmit) Submit(ctx context.Context, id session.ID, generation string, input inbox.Input) (host.Receipt, error) {
	receipt, err := f.pagedTranscriptClient.Submit(ctx, id, generation, input)
	if err != nil {
		return receipt, err
	}
	select {
	case <-f.release:
		return receipt, nil
	case <-ctx.Done():
		return host.Receipt{}, ctx.Err()
	}
}

func TestPastedExportPrivateEntryRemovedCopyAndInvalidArguments(t *testing.T) {
	f := conversationFixture("public response")
	release := make(chan struct{})
	client := &gatedExportSubmit{pagedTranscriptClient: f, release: release}
	keys := make(chan Key, 16)
	out := &screenObserver{}
	done := make(chan error, 1)
	dir := filepath.Join(t.TempDir(), "exports")
	theme := Theme{Plain: true}
	go func() {
		done <- Run(t.Context(), Config{Client: client, ID: "s", Keys: keys, Output: out, Theme: &theme, ExportDirectory: dir})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "public response") })
	keys <- Key{Text: "/export last", Paste: true}
	waitFor(t, func() bool { return strings.Contains(out.text(), "pasted:") })
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("paste exported before Enter", err)
	}
	keys <- Key{Name: "enter"}
	paths := exportFiles(t, dir, 1)
	if exportBody(t, paths[0]) != "public response" {
		t.Fatal("pasted export lost the canonical response")
	}
	waitFor(t, func() bool {
		return strings.Contains(out.text(), "exported to") && !strings.Contains(out.text(), "cancel waiting")
	})
	keys <- Key{Text: "/login openai synthetic-id"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "private: API key") })
	keys <- Key{Text: "/export last"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.logins == 1 })
	waitFor(t, func() bool { return strings.Contains(out.text(), "API key stored") })
	keys <- Key{Text: "/copy last"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.inputs) == 1 })
	// Removed commands are now normal messages. Observe completion rather
	// than interpreting an earlier idle frame as acknowledgement of Submit.
	waitFor(t, func() bool { return strings.Contains(out.text(), "cancel waiting") })
	close(release)
	waitFor(t, func() bool { return !strings.Contains(out.text(), "cancel waiting") })
	keys <- Key{Text: "/export turn 1"}
	keys <- Key{Name: "enter"}
	waitFor(t, func() bool { return strings.Contains(out.text(), "usage: /export [last | all]") })
	exportFiles(t, dir, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) != 1 || string(f.inputs[0].Payload) != `"/copy last"` || f.login != "/export last" {
		t.Fatal("command safety changed")
	}
}

func TestLiveExportPickerArrowScrollTabEscapeAndAnalysisSeparation(t *testing.T) {
	for _, size := range [][2]int{{44, 14}, {60, 20}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var texts []string
			for i := range 25 {
				texts = append(texts, fmt.Sprintf("agent-%02d 日本語", i))
			}
			f := conversationFixture(texts...)
			keys := make(chan Key, 16)
			out := &screenObserver{}
			done := make(chan error, 1)
			dir := filepath.Join(t.TempDir(), "exports")
			theme := Theme{Plain: true, ASCII: true}
			analysisExports := make(chan string, 1)
			go func() {
				done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme, ExportDirectory: dir, Size: func() (int, int) { return size[0], size[1] }, ExportAnalysis: func(_ context.Context, _ analysis.Report, format string) (string, error) {
					analysisExports <- format
					return "statistics-only.md", nil
				}})
			}()
			defer func() {
				keys <- Key{Name: "detach"}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}()
			waitFor(t, func() bool { return strings.Contains(out.text(), "agent-24") })
			keys <- Key{Text: "/export"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Export response") })
			for i := 23; i >= 0; i-- {
				keys <- Key{Name: "down"}
				waitFor(t, func() bool { return strings.Contains(out.text(), fmt.Sprintf("* Turn %d ", i+1)) })
			}
			if !strings.Contains(out.text(), "^ more") {
				t.Fatal("picker did not scroll to oldest response")
			}
			keys <- Key{Name: "up"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "* Turn 2 ") })
			keys <- Key{Name: "tab"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "* Turn 1 ") })
			keys <- Key{Name: "enter"}
			paths := exportFiles(t, dir, 1)
			waitFor(t, func() bool {
				return strings.Contains(out.text(), "exported to") && !strings.Contains(out.text(), "cancel waiting")
			})
			if exportBody(t, paths[0]) != texts[0] {
				t.Fatal("arrow/Tab selection exported wrong response")
			}
			// Analysis Export remains its own metadata/statistics-only picker.
			keys <- Key{Text: "/analyze"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Analyze") })
			for range len(analysis.Views) - 1 {
				keys <- Key{Name: "down"}
			}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "metadata/statistics") })
			keys <- Key{Name: "down"}
			keys <- Key{Name: "enter"}
			waitFor(t, func() bool { return strings.Contains(out.text(), "Analysis exported") })
			if <-analysisExports != "Markdown" {
				t.Fatal("analysis export path changed")
			}
			if files, _ := os.ReadDir(dir); len(files) != 1 {
				t.Fatal("analysis export invoked conversation exporter")
			}
		})
	}
}

func TestExportTabCommandCompletion(t *testing.T) {
	for i, want := range []string{"/export", "/export last", "/export all"} {
		u := plainUI()
		var editor Editor
		editor.replace("/ex")
		var complete completion
		complete.resetMenu(&u)
		u.Input = editor.Text()
		for range i {
			complete.move(&editor, idleSnapshot(), &u, 1)
		}
		complete.apply(&editor, idleSnapshot(), &u)
		if editor.Text() != want {
			t.Fatal("Tab completion mismatch", editor.Text(), want)
		}
	}
}

func TestLiveExportReadsCanonicalTailBeyondSubscriptionCursor(t *testing.T) {
	f := conversationFixture("initial canonical response")
	keys := make(chan Key, 8)
	out := &screenObserver{}
	done := make(chan error, 1)
	dir := filepath.Join(t.TempDir(), "exports")
	theme := Theme{Plain: true}
	go func() {
		done <- Run(t.Context(), Config{Client: f, ID: "s", Keys: keys, Output: out, Theme: &theme, ExportDirectory: dir})
	}()
	defer func() {
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}()
	waitFor(t, func() bool { return strings.Contains(out.text(), "initial canonical response") })
	newer := conversationFixture("unused fixture response", "canonical response unseen by transcript")
	f.mu.Lock()
	for _, item := range newer.view.History.Items[3:] {
		item.Sequence = sessionstore.Sequence(len(f.view.History.Items) + 1)
		f.view.History.Items = append(f.view.History.Items, item)
	}
	f.mu.Unlock()
	// Do not deliver a subscription history event. Export must independently
	// discover the Host's newest canonical prefix, rather than using UI After.
	keys <- Key{Text: "/export all"}
	keys <- Key{Name: "enter"}
	paths := exportFiles(t, dir, 1)
	waitFor(t, func() bool { return strings.Contains(out.text(), "exported to") })
	body := exportBody(t, paths[0])
	if !strings.Contains(body, "initial canonical response") || !strings.Contains(body, "canonical response unseen by transcript") || strings.Count(body, "### You\n") != 2 {
		t.Fatal("export limited to subscription's loaded range")
	}
	if strings.Contains(out.text(), "canonical response unseen by transcript") {
		t.Fatal("export altered live transcript subscription cursor")
	}
}
