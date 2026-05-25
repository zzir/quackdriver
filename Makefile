# quackdriver — local development tasks.
#
# `make test` is the headline target: build + vet + go test + every example
# against an auto-managed Quack server. `make help` lists the rest.

QUACK_HOST  ?= 127.0.0.1
QUACK_PORT  ?= 9494
QUACK_TOKEN ?= super_secret
SERVER_LOG  ?= /tmp/quack-server.log

# Examples runnable as `go run ./examples/<name>` (no separate go.mod).
EXAMPLES := basic types nested timeutc append append-rich append-struct append-enum multichunk

export QUACK_HOST QUACK_PORT QUACK_TOKEN SERVER_LOG

.PHONY: help all test test-unit test-examples test-bun build vet fmt \
        server server-bg stop dump-wire bench bun-example clean

help:
	@echo "Targets:"
	@echo "  test          build + vet + unit tests + examples (auto-manages server)"
	@echo "  test-unit     just \`go test ./...\` (no server)"
	@echo "  test-examples just the example programs (auto-manages server)"
	@echo "  test-bun      run the bun ORM example (auto-manages server)"
	@echo ""
	@echo "  build         go build ./..."
	@echo "  vet           go vet ./..."
	@echo "  fmt           gofmt -w over all .go files"
	@echo ""
	@echo "  server        start the Quack server in the foreground"
	@echo "  server-bg     start the Quack server in the background"
	@echo "  stop          kill whatever is listening on QUACK_PORT"
	@echo "  dump-wire     regenerate testdata/golden/*.bin (auto-manages server)"
	@echo "  bench         APPEND vs INSERT benchmark (auto-manages server)"
	@echo ""
	@echo "  clean         remove the server log and stop any local server"
	@echo ""
	@echo "Vars (override via env or 'make VAR=value test'):"
	@echo "  QUACK_HOST   = $(QUACK_HOST)"
	@echo "  QUACK_PORT   = $(QUACK_PORT)"
	@echo "  QUACK_TOKEN  = $(QUACK_TOKEN)"
	@echo "  SERVER_LOG   = $(SERVER_LOG)"

all: test

test: build vet test-unit test-examples

test-unit:
	@echo "==> go test ./..."
	go test ./... -count=1

test-examples:
	bash scripts/with-server.sh bash scripts/run-examples.sh $(EXAMPLES)

test-bun:
	bash scripts/with-server.sh bash -c 'cd examples/bun && go run .'

build:
	@echo "==> go build ./..."
	go build ./...

vet:
	@echo "==> go vet ./..."
	go vet ./...

fmt:
	@find . -name '*.go' -not -path './references/*' -print0 | xargs -0 gofmt -w

server:
	@QUACK_EXTENSION_REPO= bash scripts/start-quack-server.sh

server-bg:
	@if lsof -ti :$(QUACK_PORT) >/dev/null 2>&1; then \
		echo "port $(QUACK_PORT) already in use; assuming server is up"; \
	else \
		QUACK_EXTENSION_REPO= bash scripts/start-quack-server.sh >$(SERVER_LOG) 2>&1 & \
		for _ in $$(seq 1 40); do lsof -ti :$(QUACK_PORT) >/dev/null 2>&1 && break; sleep 0.25; done; \
		echo "==> server up on quack://$(QUACK_HOST):$(QUACK_PORT) (log: $(SERVER_LOG))"; \
	fi

stop:
	@if lsof -ti :$(QUACK_PORT) >/dev/null 2>&1; then \
		echo "==> killing process(es) on port $(QUACK_PORT)"; \
		lsof -ti :$(QUACK_PORT) | xargs kill 2>/dev/null || true; \
	else \
		echo "nothing listening on port $(QUACK_PORT)"; \
	fi

dump-wire:
	bash scripts/with-server.sh go run ./cmd/dump-wire

bench:
	bash scripts/with-server.sh go run ./examples/bench-append

clean: stop
	@rm -f $(SERVER_LOG)
