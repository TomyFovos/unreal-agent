package provider

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type idleClient struct {
	Client
	idle func(context.Context)
}

func (c idleClient) Idle(ctx context.Context, _ llm.Request, _ llm.RequestOptions) { c.idle(ctx) }
func (c idleClient) Close() error                                                  { return nil }

func TestProviderCloseCancelsAndJoinsIdleProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		a := &adapter{
			config:          BuildConfig{Selection: Selection{Model: Model{ID: "fixture"}, Auth: credential.Reference{Method: credential.None}}},
			idleMaintenance: true,
			processes:       &processRequests{cancels: make(map[*context.CancelFunc]context.CancelFunc)},
			factory: func(BuildConfig, credential.Material) (Client, error) {
				return idleClient{idle: func(ctx context.Context) {
					close(started)
					<-ctx.Done()
					close(canceled)
					<-release // model/process cleanup must finish before Close
				}}, nil
			},
		}
		go a.Idle(t.Context(), llm.Request{Model: llm.Model{ID: "fixture"}}, llm.RequestOptions{})
		<-started
		closed := make(chan error, 1)
		go func() { closed <- a.Close() }()
		<-canceled
		synctest.Wait()
		select {
		case err := <-closed:
			t.Fatal("Close returned before idle process cleanup", err)
		default:
		}
		close(release)
		synctest.Wait()
		if err := <-closed; err != nil {
			t.Fatal(err)
		}
	})
}

func TestProviderWithoutIdleMaintenanceDoesNotConstructClient(t *testing.T) {
	a := &adapter{factory: func(BuildConfig, credential.Material) (Client, error) {
		t.Fatal("idle work instantiated an unsupported provider")
		return nil, nil
	}}
	a.Idle(t.Context(), llm.Request{}, llm.RequestOptions{})
}
