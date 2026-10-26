.PHONY: build test check snapshot test-snapshot test-pilot-kit test-pilot-provider test-asp-oauth test-devlore-launcher test-daily-development

GORELEASER ?= goreleaser
BIN_DIR ?= bin

snapshot:
	python3 scripts/snapshot.py build --goreleaser "$(GORELEASER)"

test-snapshot:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_snapshot.py'

test-pilot-kit:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_pilot_kit.py'

test-pilot-provider:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_pilot_provider.py'

test-asp-oauth:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_asp_oauth.py'
	go test ./cmd/internal/authcli ./cmd/unreal-agent ./harness/credential ./harness/provider ./harness/llm/clients/openaicodex -run '^TestDockerSandbox'

test-devlore-launcher:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_devlore_launcher.py'
	go test ./cmd/unreal-agent -run '^TestDevlorePermanent'

test-daily-development:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_daily_development.py'

build:
	mkdir -p "$(BIN_DIR)"
	go build -trimpath -o "$(BIN_DIR)/unreal-agent" ./cmd/unreal-agent
	ln -sf unreal-agent "$(BIN_DIR)/unreal"
	go build -trimpath -o "$(BIN_DIR)/unreal-agent-runner" ./cmd/unreal-agent-runner
	go build -trimpath -o "$(BIN_DIR)/unreal-agent-auth" ./cmd/unreal-agent-auth

test:
	go test -race ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l cmd harness internal)" || { gofmt -l cmd harness internal; exit 1; }
