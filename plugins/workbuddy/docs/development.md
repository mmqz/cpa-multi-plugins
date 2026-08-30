# Development Guide

Local build, test, debug, and release workflow for the workbuddy plugin.

## Prerequisites

- Go 1.26+ (matches CPA's `go.mod`)
- CGO enabled (required for `-buildmode=c-shared`)
- For cross-compilation: platform-specific C toolchains (e.g. `aarch64-linux-gnu-gcc` for linux/arm64, `osxcross` for darwin)

## Build

```bash
# Current platform
make build

# Or explicitly
CGO_ENABLED=1 go build -buildmode=c-shared -ldflags "-X main.version=$(git describe --tags --always)" -o workbuddy.so .
```

Output: `workbuddy.so` (and `workbuddy.h` as a side effect).

## Test

```bash
# All tests with race detector
make test

# Or explicitly
go test -race -count=1 ./...

# Coverage
go test -cover ./...
go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out
```

The test suite (115 tests at the time of writing) covers:

- Cache merge logic (credits / plan / checkin never wiped on fast paths)
- Alias reverse resolution (client alias → upstream model id)
- tool_choice normalization (object → string, `none` suppresses tools)
- SSE chunk cleaning (empty tool_call shells stripped)
- UID sanitization for auth file names (path traversal defense)
- Scheduler pick behavior (sticky + fallback + `scheduler_mode: off` defers)
- Credits lifecycle transitions (exhausted → disable / delete / re-enable)

## Lint

```bash
make lint
```

Runs `gofmt -l`, `go vet`, and (if installed) `staticcheck`, `gocritic`, `unparam`.

Install optional linters:

```bash
go install honnef.co/go/tools/cmd/staticcheck@latest
go install github.com/go-critic/go-critic/cmd/gocritic@latest
go install mvdan.cc/unparam@latest
go install github.com/fzipp/gocyclo/cmd/gocyclo@latest
```

## Local debug

The plugin can't run standalone — it must be loaded by CPA. The fastest
iteration loop:

```bash
# 1. Build
make build

# 2. Copy to CPA's plugin dir (adjust path to your CPA install)
cp workbuddy.so /path/to/cliproxyapi/plugins/

# 3. Restart CPA
docker restart cpa-manager-plus-cli-proxy-api-1
# or: systemctl restart cliproxyapi

# 4. Tail logs
docker logs -f cpa-manager-plus-cli-proxy-api-1 | grep -i workbuddy
```

Quick smoke test:

```bash
# Non-streaming
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<alias>","messages":[{"role":"user","content":"hi"}],"max_tokens":5}'

# Streaming
curl -N http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"<alias>","messages":[{"role":"user","content":"hi"}],"max_tokens":5,"stream":true}'

# Panel API
curl http://localhost:8317/v0/management/plugins/workbuddy/accounts \
  -H "Authorization: Bearer $MGMT_KEY"
```

## Release

```bash
# Tag and push
make tag VERSION=0.8.0

# Build multi-arch zips (requires cross C toolchains)
make release
```

Cross-compilation notes:

- `linux/amd64` and `linux/arm64` work on a Linux host with
  `gcc-x86-64-linux-gnu` and `gcc-aarch64-linux-gnu` installed.
- `darwin/*` requires [osxcross](https://github.com/tpoechtrager/osxcross).
- `windows/amd64` c-shared is not currently supported by Go (tracked in
  golang/go#23609); skip it.

## Project layout conventions

- One responsibility per file, ≤1500 lines per file, ≤200 lines per function
- Comments in English (community standard); user-visible strings may be Chinese
- No new dependencies without discussion — `go.mod` currently only has CPA SDK
- Wire formats are sacred: never change `pluginapi` / `pluginabi` field names
- Every commit must keep `go test -race ./...` green
