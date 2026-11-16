# `make build` stamps the binary with its git SHA and build time so the server
# and the web/ directory it serves can be compared (GET /api/rc/version).
SHA   := $(shell git rev-parse --short=7 HEAD)
DIRTY := $(shell test -n "$$(git status --porcelain -- . ':!deck-remote' ':!.worktrees')" && echo true || echo false)
AT    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.buildSHA=$(SHA) -X main.buildAt=$(AT) -X main.buildDirty=$(DIRTY)

.PHONY: build test
build:
	go build -ldflags "$(LDFLAGS)" -o deck-remote .

test:
	go test ./... && node --test web/

# Browser end-to-end: system Chrome (full suite), WebKit (mock fixtures).
# Serial on purpose: in parallel, five Chromes plus two Go builds starve each other on a loaded
# machine and a different test times out each run. Slow machine: E2E_TIMEOUT_MS=60000 make e2e.
.PHONY: e2e e2e-webkit e2e-webkit-deps e2e-deps
e2e: e2e-deps
	cd e2e && npm test

e2e-webkit: e2e-deps
	cd e2e && npm run test:webkit

# Explicit download; stays inside this worktree's ignored node_modules.
e2e-webkit-deps: e2e-deps
	cd e2e && npm run install:webkit

e2e-deps: e2e/node_modules/.package-lock.json

e2e/node_modules/.package-lock.json: e2e/package.json e2e/package-lock.json
	cd e2e && npm ci
