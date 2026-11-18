package host_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/subagent"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/internal/testsession"
)

type archiveBenchmarkModel struct{}

func (archiveBenchmarkModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return llm.Response{}, fmt.Errorf("startup must not perform inference")
}

type archiveStartupPhases struct {
	mu    sync.Mutex
	phase map[string]time.Duration
	ready chan struct{}
}

func (*archiveStartupPhases) Enabled(context.Context, slog.Level) bool { return true }
func (p *archiveStartupPhases) Handle(_ context.Context, r slog.Record) error {
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
	p.mu.Lock()
	defer p.mu.Unlock()
	if name != "" {
		p.phase[name] += elapsed
	}
	return nil
}
func (p *archiveStartupPhases) WithAttrs([]slog.Attr) slog.Handler { return p }
func (p *archiveStartupPhases) WithGroup(string) slog.Handler      { return p }

// This benchmark uses only pre-archive public APIs, so the identical fixture
// and command also measure d82b00c. Warmup includes the one-time migration;
// timed opens measure subsequent starts of the same 2,000-child state.
func BenchmarkSettledChildStartup(b *testing.B) {
	b.StopTimer()
	dir := filepath.Join(b.TempDir(), "sessions")
	testsession.WriteSettledChildren(b, dir, 2000, 128<<10)
	previous := slog.Default()
	b.Cleanup(func() { slog.SetDefault(previous) })
	phases := &archiveStartupPhases{phase: make(map[string]time.Duration), ready: make(chan struct{}, 1)}
	slog.SetDefault(slog.New(phases))
	open := func() (*host.Host, time.Duration) {
		start := time.Now()
		h, err := host.New(b.Context(), host.Config{Directory: dir, Build: func(ctx context.Context, _ session.ID) (host.Runtime, error) {
			owner, _ := host.SessionFromContext(ctx)
			children, err := subagent.NewManager(ctx, subagent.Config{Owner: owner, Directory: dir})
			if err != nil {
				return host.Runtime{}, err
			}
			count := 0
			var reconcile time.Duration
			return host.Runtime{Builder: contextbuilder.NewBuilder(), LLM: archiveBenchmarkModel{}, Tools: tool.NewRegistry(tool.StaticTranslators{}), Operations: operation.NewLocalOperationManager(ctx), Close: children.Close,
				InterruptedInput: children.InterruptedInput, CompletedInput: children.CompletedInput,
				RetainOperation: func(ctx context.Context, op operation.Operation) (bool, error) {
					start := time.Now()
					live, err := children.RetainOperation(ctx, op)
					reconcile += time.Since(start)
					count++
					if count == 2000 {
						phases.mu.Lock()
						phases.phase["host.executor_restore"] += reconcile
						phases.mu.Unlock()
						phases.ready <- struct{}{}
					}
					return live, err
				}}, nil
		}})
		if err != nil {
			b.Fatal(err)
		}
		if _, err = h.Resume(b.Context(), host.Options{ID: "main"}); err != nil {
			h.Close()
			b.Fatal(err)
		}
		select {
		case <-phases.ready:
		case <-b.Context().Done():
			b.Fatal(b.Context().Err())
		}
		return h, time.Since(start)
	}
	warmup := time.Now()
	h, _ := open()
	if err := h.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(time.Since(warmup).Nanoseconds())/1e6, "migration-start-ms")
	var liveBytes int64
	var liveRecords int
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		liveBytes += info.Size()
		if strings.HasSuffix(path, ".session.jsonl") {
			liveRecords++
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(liveBytes), "live-B")
	b.ReportMetric(float64(liveRecords), "live-records")
	phases.mu.Lock()
	clear(phases.phase)
	phases.mu.Unlock()
	var elapsed time.Duration
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		h, duration := open()
		elapsed += duration
		b.StopTimer()
		if err := h.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N)/1e6, "start-ms")
	phases.mu.Lock()
	defer phases.mu.Unlock()
	for name, elapsed := range phases.phase {
		b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N)/1e6, name+"-ms")
	}
}
