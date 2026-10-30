package tui

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/viewer"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

type startupAdapter struct{}

func (startupAdapter) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{}, fmt.Errorf("unexpected inference during startup")
}

type startupClient struct{ owner *host.Session }

func (c startupClient) Inspect(_ context.Context, _ session.ID, after sessionstore.Sequence, limit int) (host.View, error) {
	return c.owner.Inspect(after, limit)
}
func (c startupClient) InspectBefore(_ context.Context, _ session.ID, before sessionstore.Sequence, limit int) (host.View, error) {
	return c.owner.InspectBefore(before, limit)
}
func (c startupClient) Subscribe(_ context.Context, _ session.ID, after sessionstore.Sequence, limit, capacity int) (host.Subscription, error) {
	return c.owner.Subscribe(after, limit, capacity)
}
func (c startupClient) Open(context.Context, host.Mode, session.ID) (host.View, error) {
	return c.owner.Inspect(0, 1)
}
func (c startupClient) Submit(ctx context.Context, _ session.ID, gen string, input inbox.Input) (host.Receipt, error) {
	return c.owner.Submit(ctx, gen, input)
}

func startupReadBytes() uint64 {
	b, _ := os.ReadFile("/proc/self/io")
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "rchar: "); ok {
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

func BenchmarkSessionStartup(b *testing.B) {
	previousLogger := slog.Default()
	phases := &startupPhases{elapsed: make(map[string]time.Duration)}
	slog.SetDefault(slog.New(phases))
	b.Cleanup(func() { slog.SetDefault(previousLogger) })
	dir := b.TempDir()
	logs := testsession.StartupLogs(b, dir, map[string]any{})
	b.ReportAllocs()
	var openTime, viewerTime, frameTime, firstScreenTime, inputTime time.Duration
	var readBytes uint64
	for b.Loop() {
		b.StopTimer()
		for name, data := range logs {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
				b.Fatal(err)
			}
		}
		goruntime.GC()
		before := startupReadBytes()
		b.StartTimer()
		start := time.Now()
		h, err := host.New(b.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
			return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: startupAdapter{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx)}, nil
		}})
		if err != nil {
			b.Fatal(err)
		}
		s, err := h.Resume(b.Context(), host.Options{ID: "main"})
		if err != nil {
			h.Close()
			b.Fatal(err)
		}
		opened := time.Now()
		vmodel := viewer.New(viewer.SubagentOptions())
		vclient := viewer.NewClient(viewer.ParentReader{Host: h, ParentID: s.ID, Directory: dir}, vmodel, nil)
		if err := vclient.Refresh(b.Context(), s.ID); err != nil {
			h.Close()
			b.Fatal(err)
		}
		viewed := time.Now()
		keys := make(chan Key, 2)
		out := &startupScreen{ready: make(chan struct{}, 1), typed: make(chan struct{}, 1)}
		done := make(chan error, 1)
		go func() {
			done <- Run(b.Context(), Config{Client: startupClient{s}, ID: s.ID, Keys: keys, Output: out, Size: func() (int, int) { return 140, 32 }, Theme: &Theme{Plain: true}, Viewer: func(now time.Time) viewer.PanelSnapshot {
				return viewer.PanelSnapshot{ParentID: s.ID, Rows: vmodel.Rows(now)}
			}})
		}()
		<-out.ready
		framed := time.Now()
		keys <- Key{Text: "startup-input-probe"}
		<-out.typed
		typed := time.Now()
		readBytes += startupReadBytes() - before
		openTime += opened.Sub(start)
		viewerTime += viewed.Sub(opened)
		frameTime += framed.Sub(viewed)
		firstScreenTime += framed.Sub(start)
		inputTime += typed.Sub(start)
		keys <- Key{Name: "detach"}
		if err := <-done; err != nil {
			b.Fatal(err)
		}
		h.Close()
	}
	b.ReportMetric(float64(openTime.Nanoseconds())/float64(b.N)/1e6, "open-ms")
	b.ReportMetric(float64(viewerTime.Nanoseconds())/float64(b.N)/1e6, "viewer-ms")
	b.ReportMetric(float64(frameTime.Nanoseconds())/float64(b.N)/1e6, "tui-ms")
	b.ReportMetric(float64(inputTime.Nanoseconds())/float64(b.N)/1e6, "input-ms")
	b.ReportMetric(float64(firstScreenTime.Nanoseconds())/float64(b.N)/1e6, "first-screen-ms")
	b.ReportMetric(float64(readBytes)/float64(b.N), "read-B")
	b.ReportMetric(float64(len(logs["main.session.jsonl"])), "main-B")
	for name, elapsed := range phases.elapsed {
		b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N)/1e6, name+"-ms")
	}
}

type startupPhases struct {
	mu      sync.Mutex
	elapsed map[string]time.Duration
}

func (*startupPhases) Enabled(context.Context, slog.Level) bool { return true }
func (p *startupPhases) Handle(_ context.Context, r slog.Record) error {
	var name string
	var elapsed time.Duration
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "phase" {
			name = a.Value.String()
		}
		if a.Key == "elapsed" {
			elapsed = a.Value.Duration()
		}
		return true
	})
	if name != "" {
		p.mu.Lock()
		p.elapsed[name] += elapsed
		p.mu.Unlock()
	}
	return nil
}
func (p *startupPhases) WithAttrs([]slog.Attr) slog.Handler { return p }
func (p *startupPhases) WithGroup(string) slog.Handler      { return p }

type startupScreen struct{ ready, typed chan struct{} }

func (s *startupScreen) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("startup-entry-1999")) {
		select {
		case s.ready <- struct{}{}:
		default:
		}
	}
	if bytes.Contains(p, []byte("startup-input-probe")) {
		select {
		case s.typed <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}
