# Hexagon build targets. The server embeds web/dist, so `make build` always
# builds the frontend first.

GO      ?= go
NPM     ?= npm
BIN     := bin/hexagon
WEB     := web

.PHONY: all build build-web build-server dist-placeholder dev dev-server dev-web test fmt vet clean

all: build

build: build-web build-server

build-web:
	cd $(WEB) && $(NPM) run build

build-server: dist-placeholder
	$(GO) build -o $(BIN) ./cmd/hexagon

# The server embeds web/dist, so the directory must exist even before the
# frontend has ever been built (a fresh clone, or `go test ./...`).
dist-placeholder:
	@mkdir -p $(WEB)/dist && touch $(WEB)/dist/.gitkeep

# Runs the Go server and the Vite dev server together. Open http://localhost:5173,
# which proxies /api to the Go process on :8080. Ctrl-C stops both.
dev:
	@trap 'kill 0' EXIT INT TERM; \
	$(MAKE) dev-server & \
	$(MAKE) dev-web & \
	wait

# The browser talks to Vite on :5173, so that is the public origin in dev: it is
# what the OAuth callback and the Origin check must agree on.
dev-server:
	HEXAGON_DEBUG=1 HEXAGON_PUBLIC_URL=http://localhost:5173 $(GO) run ./cmd/hexagon

dev-web:
	cd $(WEB) && $(NPM) run dev

test: dist-placeholder
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BIN) $(WEB)/dist
	$(MAKE) dist-placeholder
