.PHONY: build test check test-claude-structured-unit test-claude-structured snapshot test-snapshot test-pilot-kit test-pilot-provider test-asp-oauth test-devlore-launcher test-daily-development

GORELEASER ?= goreleaser

snapshot:
	python3 scripts/snapshot.py build --goreleaser "$(GORELEASER)"

test-snapshot:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_snapshot.py'

test-pilot-kit:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_pilot_kit.py'

test-pilot-provider:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_pilot_provider.py'
	go test ./cmd/unreal-agent -run '^TestPilotProvider'

test-asp-oauth:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_asp_oauth.py'
	go test ./cmd/internal/authcli ./cmd/unreal-agent ./harness/credential ./harness/provider ./harness/llm/clients/openaicodex -run '^TestDockerSandbox'

test-devlore-launcher:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_devlore_launcher.py'
	go test ./cmd/unreal-agent -run '^TestDevlorePermanent'

test-daily-development:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_daily_development.py'
	go test ./cmd/unreal-agent -run '^TestDailyDevelopment'

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
