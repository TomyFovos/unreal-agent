.PHONY: build test check test-claude-structured-unit test-claude-structured

build:
	go build -trimpath -o bin/unreal-agent ./cmd/unreal-agent
	ln -sf unreal-agent bin/unreal
	go build -trimpath -o bin/unreal-agent-runner ./cmd/unreal-agent-runner
	go build -trimpath -o bin/unreal-agent-auth ./cmd/unreal-agent-auth

test:
	go test -race ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l cmd harness internal)" || { gofmt -l cmd harness internal; exit 1; }

# No Claude process/auth/SDK dependency: schema, requests, corpus, errors, round
# controller, then existing Registry/Permission/Operation/context boundaries.
test-claude-structured-unit:
	go test ./harness/llm/clients/claudecode -run '^Test(Structured(Layer|Assistant|MultipleSerializer|ResultValidation|StreamStage|PreinitStages|FinalResultAuthoritative|ResultBound|SchemaSnapshot)|SDKAssistantErrorsAreClosedAndTyped|SDKAssistantErrorDoesNotClassifyFreeText)'
	go test ./harness/coordinator ./harness/tool/... ./harness/operation ./harness/native ./harness/permission ./harness/contextbuilder ./harness/contextengine

# Unit gate always precedes fake CLI/Host integration. Real inference is never
# included; TestStructuredRealAdapterSchemaProbe skips without explicit flags.
test-claude-structured: test-claude-structured-unit
	go test ./harness/llm/clients/claudecode -run '^Test(Structured(OfficialControl|SchemaAndInvalidProtocol|CannotExecute|PolicyDenial|LaunchAndConfig|RoundLimit|ProviderCrash|ProviderUsesPublicAction)|SDKAssistantErrorsAlsoFailClosedInMCP)'
	go test ./cmd/unreal-agent -run '^TestClaudeStructured'
