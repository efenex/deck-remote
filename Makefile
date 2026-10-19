# `make build` stamps the binary with its git SHA and build time so the server
# and the web/ directory it serves can be compared (GET /api/rc/version).
SHA   := $(shell git rev-parse --short=7 HEAD)
DIRTY := $(shell test -n "$$(git status --porcelain -- . ':!deck-remote')" && echo true || echo false)
AT    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.buildSHA=$(SHA) -X main.buildAt=$(AT) -X main.buildDirty=$(DIRTY)

.PHONY: build test
build:
	go build -ldflags "$(LDFLAGS)" -o deck-remote .

test:
	go test ./... && node --test web/
