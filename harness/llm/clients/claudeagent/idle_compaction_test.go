package claudeagent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func idleCompactionFixture(t *testing.T, tokens int64) (*Client, llm.RequestOptions, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	client := &Client{config: Config{Directory: t.TempDir()}, contextTokens: map[string]int64{}}
	opt := llm.RequestOptions{
		Identity:           llm.RequestIdentity{SessionID: "idle-test"},
		ConversationTokens: map[string]int64{ProviderID: tokens},
		StartBackground: func(work func(context.Context)) bool {
			if ctx.Err() != nil {
				return false
			}
			workers.Go(func() { work(ctx) })
			return true
		},
	}
	return client, opt, func() { cancel(); workers.Wait() }
}

func assertNoIdleCompaction(t *testing.T, calls <-chan time.Time) {
	t.Helper()
	select {
	case at := <-calls:
		t.Fatal("unexpected compaction at", at)
	default:
	}
}

func TestIdleCompactionAt50MinutesAcrossClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, opt, closeOwner := idleCompactionFixture(t, 120_001)
		defer closeOwner()
		calls := make(chan time.Time, 2)
		session := sessionUUID("", opt.Identity.SessionID)
		state := client.idleCompactionState(session, opt)
		state.beginTurn()
		state.endTurn()
		lastCall := time.Now()
		time.Sleep(10 * time.Minute) // Idle may follow a long tool/operation wait.
		idle, cancelIdle := context.WithCancel(t.Context())
		client.scheduleIdleCompaction(idle, opt, func(ctx context.Context, got string, before int64) {
			if got != session || before != 120_001 || ctx.Err() != nil {
				t.Error("lost conversation, size or owner context", got, before, ctx.Err())
			}
			calls <- time.Now()
		})
		cancelIdle() // The transient Idle context must not own the timer.
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
		next := &Client{config: client.config}
		next.scheduleIdleCompaction(t.Context(), opt, func(context.Context, string, int64) {
			t.Error("duplicate Idle replaced or repeated a pending compact")
		})
		synctest.Wait()
		time.Sleep(39 * time.Minute)
		synctest.Wait()
		assertNoIdleCompaction(t, calls)
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case at := <-calls:
			if at.Sub(lastCall) != 50*time.Minute {
				t.Fatal("timer measured from Idle rather than the last call", at.Sub(lastCall))
			}
		default:
			t.Fatal("no compaction after 50 minutes")
		}
		next.scheduleIdleCompaction(t.Context(), opt, func(context.Context, string, int64) { t.Error("compacted twice without a new turn") })
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		assertNoIdleCompaction(t, calls)
	})
}

func TestIdleCompactionNewTurnDisarmsAndRestartsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, opt, closeOwner := idleCompactionFixture(t, 250_000)
		defer closeOwner()
		calls := make(chan time.Time, 2)
		run := func(context.Context, string, int64) { calls <- time.Now() }
		client.scheduleIdleCompaction(t.Context(), opt, run)
		time.Sleep(49 * time.Minute)
		state := client.idleCompactionState(sessionUUID("", opt.Identity.SessionID), opt)
		state.beginTurn()
		client.scheduleIdleCompaction(t.Context(), opt, run) // A late Idle cannot arm during Respond.
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		assertNoIdleCompaction(t, calls)
		state.endTurn()
		lastCall := time.Now()
		client.scheduleIdleCompaction(t.Context(), opt, run)
		time.Sleep(49 * time.Minute)
		synctest.Wait()
		assertNoIdleCompaction(t, calls)
		time.Sleep(time.Minute)
		synctest.Wait()
		if at := <-calls; at.Sub(lastCall) != 50*time.Minute {
			t.Fatal("new turn did not reset the deadline", at.Sub(lastCall))
		}
	})
}

func TestIdleCompactionRequiresLargeContextAndOwner(t *testing.T) {
	for _, tokens := range []int64{0, 119_999, 120_000} {
		t.Run(fmt.Sprint(tokens), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, opt, closeOwner := idleCompactionFixture(t, tokens)
				defer closeOwner()
				client.scheduleIdleCompaction(t.Context(), opt, func(context.Context, string, int64) { t.Error("small context compacted") })
				time.Sleep(2 * time.Hour)
			})
		})
	}
	synctest.Test(t, func(t *testing.T) {
		client, opt, closeOwner := idleCompactionFixture(t, 250_000)
		defer closeOwner()
		run := func(context.Context, string, int64) { t.Error("work started without a live owner/Idle") }
		start := opt.StartBackground
		opt.StartBackground = nil
		client.scheduleIdleCompaction(t.Context(), opt, run)
		opt.StartBackground = start
		idle, cancel := context.WithCancel(t.Context())
		cancel()
		client.scheduleIdleCompaction(idle, opt, run)
		opt.StartBackground = func(func(context.Context)) bool { return false }
		client.scheduleIdleCompaction(t.Context(), opt, run)
		time.Sleep(2 * time.Hour)
		idleCompactions.Lock()
		defer idleCompactions.Unlock()
		if idleCompactions.entries[client.config.Directory+"\x00"+sessionUUID("", opt.Identity.SessionID)] != nil {
			t.Fatal("refused work left a conversation registered")
		}
	})
}

func TestIdleCompactionRunningWorkMakesNewTurnWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, opt, closeOwner := idleCompactionFixture(t, 250_000)
		defer closeOwner()
		started, release, entered := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
		client.scheduleIdleCompaction(t.Context(), opt, func(ctx context.Context, _ string, _ int64) {
			started <- ctx
			<-release
		})
		time.Sleep(50 * time.Minute)
		ctx := <-started
		session := sessionUUID("", opt.Identity.SessionID)
		state := client.idleCompactionState(session, opt)
		go func() {
			state.beginTurn()
			defer state.endTurn()
			unlock, err := client.lockConversation(t.Context(), session)
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			close(entered)
		}()
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatal("new turn canceled a running compaction")
		}
		select {
		case <-entered:
			t.Fatal("new turn overlapped compaction")
		default:
		}
		close(release)
		<-entered
		synctest.Wait()
	})
}

func TestIdleCompactionNewTurnWinsBeforeConversationLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, opt, closeOwner := idleCompactionFixture(t, 250_000)
		defer closeOwner()
		session := sessionUUID("", opt.Identity.SessionID)
		unlock, err := client.lockConversation(t.Context(), session)
		if err != nil {
			t.Fatal(err)
		}
		client.scheduleIdleCompaction(t.Context(), opt, func(context.Context, string, int64) { t.Error("stale timer compacted after new input") })
		time.Sleep(50 * time.Minute)
		synctest.Wait() // Timer expired, but the conversation is still owned.
		state := client.idleCompactionState(session, opt)
		state.beginTurn()
		unlock()
		synctest.Wait()
		state.endTurn()
	})
}

func TestIdleCompactionOwnerCancellationRemovesTimersAndWorkers(t *testing.T) {
	for _, phase := range []string{"timer", "lock_wait", "compacting"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, opt, closeOwner := idleCompactionFixture(t, 250_000)
				session := sessionUUID("", opt.Identity.SessionID)
				key := client.config.Directory + "\x00" + session
				if phase == "lock_wait" {
					unlock, err := client.lockConversation(t.Context(), session)
					if err != nil {
						t.Fatal(err)
					}
					defer unlock()
				}
				started := make(chan struct{})
				client.scheduleIdleCompaction(t.Context(), opt, func(ctx context.Context, _ string, _ int64) {
					close(started)
					<-ctx.Done()
				})
				if phase != "timer" {
					time.Sleep(50 * time.Minute)
					synctest.Wait()
				}
				if phase == "compacting" {
					<-started
				}
				closeOwner()
				idleCompactions.Lock()
				_, remains := idleCompactions.entries[key]
				idleCompactions.Unlock()
				if remains {
					t.Fatal("owner returned with a timer/worker still registered")
				}
				time.Sleep(2 * time.Hour)
				if phase != "compacting" {
					select {
					case <-started:
						t.Fatal("work started after owner shutdown")
					default:
					}
				}
			})
		})
	}
}
