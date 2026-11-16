.PHONY: build test check prepush snapshot test-snapshot test-pilot-kit test-pilot-provider test-asp-oauth test-daily-development mac-install

GORELEASER ?= goreleaser
BIN_DIR ?= bin
export BASE

# Distributor choice, compiled into every product executable.
OPENCODE_GO_ENABLED ?= true
BUILD_FEATURE_FLAGS = $(if $(filter false,$(OPENCODE_GO_ENABLED)),-tags=disable_opencode_go)

snapshot:
	python3 scripts/snapshot.py build --goreleaser "$(GORELEASER)" $(if $(filter false,$(OPENCODE_GO_ENABLED)),--disable-opencode-go)

test-snapshot:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_snapshot.py'

test-pilot-kit:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_pilot_kit.py'

test-pilot-provider:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_pilot_provider.py'

test-asp-oauth:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_asp_oauth.py'
	go test ./cmd/internal/authcli ./cmd/unreal-agent ./harness/credential ./harness/provider ./harness/llm/clients/openaicodex -run '^TestDockerSandbox'

test-daily-development:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_daily_development.py'

mac-install:
	@test -n "$(REV)" || { echo "REV is required: make mac-install REV=<commit>" >&2; exit 2; }
	sh scripts/mac/install.sh "$(REV)" $(if $(filter false,$(OPENCODE_GO_ENABLED)),--disable-opencode-go)

build:
	mkdir -p "$(BIN_DIR)"
	go build $(BUILD_FEATURE_FLAGS) -trimpath -o "$(BIN_DIR)/unreal-agent" ./cmd/unreal-agent
	ln -sf unreal-agent "$(BIN_DIR)/unreal"
	go build $(BUILD_FEATURE_FLAGS) -trimpath -o "$(BIN_DIR)/unreal-agent-runner" ./cmd/unreal-agent-runner
	go build $(BUILD_FEATURE_FLAGS) -trimpath -o "$(BIN_DIR)/unreal-agent-auth" ./cmd/unreal-agent-auth

test:
	go test -race ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l cmd harness internal)" || { gofmt -l cmd harness internal; exit 1; }

prepush:
	python3 scripts/prepush.py
