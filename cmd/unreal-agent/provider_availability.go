package main

import (
	"context"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/credential"
	"github.com/unreallabsai/unreal-agent/harness/host"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/modelcatalog"
	"github.com/unreallabsai/unreal-agent/harness/provider"
)

// This local startup state performs no inference and is hidden from /model.
// A real provider is selected explicitly after login, using the usual binding.
const loginProvider = "login-required"
const loginModel = "ログイン待ち"
const loginInstructions = "利用できる接続先がありません。ChatGPT: Bash で codex login。Claude: Bash で claude を起動し、その中で /login。CLI がなければ先にインストールしてください。ログイン後は /model refresh で接続先を選んでください。インストールのやり直しは不要です。"

func loginRuntime() agentrunner.ProviderRuntime {
	return agentrunner.ProviderRuntime{Provider: provider.Selection{Version: 1, Provider: loginProvider, Model: provider.Model{ID: loginModel}, Auth: credential.Reference{Method: credential.None}, MaxAttempts: 1, Source: "local login instructions"}, ReasoningEffort: llm.ReasoningEffortMedium}
}

func loginDescriptor() provider.Descriptor {
	return provider.Descriptor{ID: loginProvider, Models: []provider.Model{{ID: loginModel}}, AuthMethods: []credential.Method{credential.None}, LocalProcess: true, New: func(provider.BuildConfig, credential.Material) (provider.Client, error) {
		return loginClient{}, nil
	}}
}

type loginClient struct{}

func (loginClient) Close() error { return nil }
func (loginClient) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	return loginResponse(loginInstructions), nil
}

func loginResponse(message string) llm.Response {
	return llm.Response{Stop: llm.StopComplete, Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: message}}}}
}

func providerAvailable(ctx context.Context, runtime agentrunner.RuntimeConfig, id string) bool {
	for _, status := range runtime.Discovered.Statuses() {
		if status.ID == id && status.Availability != "available" {
			return false
		}
	}
	if runtime.CheckProvider(ctx, id) != nil {
		return false
	}
	catalog, err := runtime.Catalog(ctx, id)
	return err == nil && catalog.Available
}

// Return actionable guidance through the ordinary conversation when a selected
// provider loses authentication. The selection and its children stay unchanged.
type availabilityAdapter struct {
	llm.Adapter
	runtime agentrunner.RuntimeConfig
	id      string
}

func (a availabilityAdapter) RequiresFullToolBatch() bool { return llm.RequiresToolBatch(a.Adapter) }

func (a availabilityAdapter) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	if owner, ok := host.SessionFromContext(ctx); ok {
		if active, _ := owner.RuntimeSelection(); active != nil {
			a.id = active.Provider
		}
	}
	if a.id == loginProvider {
		ids := a.runtime.Discovered.IDs()
		for id := range a.runtime.Backends {
			ids = append(ids, id)
		}
		for _, id := range ids {
			if providerAvailable(ctx, a.runtime, id) {
				return loginResponse("利用できる接続先があります。/model で接続先とモデルを選んでください。"), nil
			}
		}
	} else if err := a.runtime.CheckProvider(ctx, a.id); err != nil {
		backend, _ := a.runtime.ProviderRuntime(a.id)
		reason := providerFailureReason(err, isDockerSandboxCodex(backend.Provider.Auth))
		return loginResponse(modelcatalog.ProviderName(a.id) + " を利用できません。" + reason.ProviderHint(a.id) + "。/login で手順を確認し、ログイン後は /model refresh を実行してください。"), nil
	}
	return a.Adapter.Respond(ctx, request, options)
}

func (a availabilityAdapter) Idle(ctx context.Context, request llm.Request, options llm.RequestOptions) {
	if idle, ok := a.Adapter.(llm.IdleAdapter); ok {
		idle.Idle(ctx, request, options)
	}
}
