# quackdriver — development tasks.
#
# test-integration, dump-wire and bench need the DuckDB CLI v2.0.0+ (Quack
# protocol v3) with the quack extension; DUCKDB_BIN names it when it is not
# `duckdb` on PATH (an absolute path: the tests run in their package
# directories). Every test server runs QUACK_TEST_INIT_SQL (default
# "INSTALL quack; LOAD quack;") before quack_serve; a CLI without httpfs needs
#
#   make test-integration DUCKDB_BIN=$PWD/duckdb \
#     QUACK_TEST_INIT_SQL="SET force_mbedtls_unsafe = 'true'; INSTALL quack; LOAD quack;"
#
# dump-wire and bench use the server on QUACK_HOST:QUACK_PORT (default
# 127.0.0.1:9494, token QUACK_TOKEN = super_secret) when one answers there;
# otherwise scripts/with-server.sh starts one and stops it afterwards.

.PHONY: test test-integration lint server dump-wire bench

# Unit tests against the fake server in internal/quacktest: no DuckDB needed.
test:
	go test -count=1 ./...

# The unit tests plus the integration suite: each package starts its own
# DuckDB CLIs (internal/livetest); integration/examples builds and runs every
# example program.
test-integration:
	go test -count=1 -tags integration ./...

# What CI runs besides the tests.
lint:
	go vet ./...
	golangci-lint run ./...

# A Quack server in the foreground; Ctrl-C stops it.
server:
	bash scripts/start-quack-server.sh

# Re-capture testdata/golden/*.bin (see testdata/golden/README.md). The
# fixtures hold TIMESTAMPTZ values rendered in the server's time zone, so the
# server runs in UTC; dump-wire refuses one that does not.
dump-wire:
	TZ=UTC bash scripts/with-server.sh go run ./cmd/dump-wire

# Per-row INSERT vs multi-row INSERT vs Conn.Append vs Appender.
bench:
	bash scripts/with-server.sh go run ./examples/bench-append
