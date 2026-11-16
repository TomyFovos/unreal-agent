.PHONY: sync check test build smoke

REVISION ?= HEAD
RUNNER ?= unreal-agent-runner
ARCH ?= amd64
OPENCODE_GO_ENABLED ?= true
BUNDLE ?= ../../bin/harbor/$(shell git rev-parse --short $(REVISION))

sync:
	uv sync --locked --all-extras

check:
	uv run --locked ruff check .
	uv run --locked ruff format --check .

test:
	uv run --locked python -m unittest discover -s tests -v

build:
	python3 build.py --revision "$(REVISION)" --runner "$(RUNNER)" --arch "$(ARCH)" --output "$(BUNDLE)" $(if $(filter false,$(OPENCODE_GO_ENABLED)),--disable-opencode-go)

smoke:
	uv run --locked python tests/smoke.py --bundle $(BUNDLE)
