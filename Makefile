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

# Browser end-to-end (run-bang phase 2 #7): system Chrome via playwright-core.
# Serial on purpose: in parallel, five Chromes plus two Go builds starve each other on a loaded
# machine and a different test times out each run. Slow machine: E2E_TIMEOUT_MS=60000 make e2e.
.PHONY: e2e e2e-deps
e2e: e2e-deps
	cd e2e && node --test --test-concurrency=1 runbang.e2e.mjs pair.e2e.mjs offline.e2e.mjs photos.e2e.mjs composer.e2e.mjs

e2e-deps: e2e/node_modules/.package-lock.json

e2e/node_modules/.package-lock.json: e2e/package.json e2e/package-lock.json
	cd e2e && npm ci
