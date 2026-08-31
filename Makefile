# Hexagon build targets. The server embeds web/dist, so `make build` always
# builds the frontend first.

GO      ?= go
NPM     ?= npm
BIN     ?= bin/hexagon
WEB     := web

# The one source of truth for the version: the file is what the binary is
# stamped with, what the package is versioned with, and what the release tag and
# the asset names are built from.
VERSION := $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Where `make install` puts the layout. PREFIX=/usr is what the package and the
# release tarball are built with; DESTDIR stages it somewhere else first.
PREFIX  ?= /usr
DESTDIR ?=

# Release builds happen inside Debian: the host has no dpkg-deb, and its Go
# toolchain links against its own libc. ARCH is a Debian architecture, mapped to
# GOARCH by the build script.
ARCH           ?= amd64
BUILD_IMAGE    ?= golang:1.26-bookworm
RELEASE_ARCHES ?= amd64 arm64
ARTIFACTS      ?= all

.PHONY: all build build-web build-server dist-placeholder dev dev-server dev-web \
	test fmt vet clean install artifacts deb release

all: build

build: build-web build-server

build-web:
	cd $(WEB) && $(NPM) run build

build-server: dist-placeholder
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/hexagon

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

# The single definition of where an installed Hexagon lives. The package stages
# it with DESTDIR, the release tarball is that same stage rolled up, and
# install.sh unpacks it — so none of the three can disagree about where the
# binary goes. It installs $(BIN) rather than building it, because the release
# build produces one binary per architecture outside the working tree.
install:
	@test -x $(BIN) || { echo "make install: $(BIN) does not exist; run make build first" >&2; exit 1; }
	install -D -m 0755 $(BIN) $(DESTDIR)$(PREFIX)/bin/hexagon
	install -D -m 0644 packaging/hexagon.service $(DESTDIR)/lib/systemd/system/hexagon.service
	install -D -m 0644 packaging/deb/copyright $(DESTDIR)$(PREFIX)/share/doc/hexagon/copyright
	install -D -m 0644 packaging/deb/changelog.Debian $(DESTDIR)$(PREFIX)/share/doc/hexagon/changelog.Debian
	install -D -m 0644 README.md $(DESTDIR)$(PREFIX)/share/doc/hexagon/README.md
	install -D -m 0644 config.example.json $(DESTDIR)$(PREFIX)/share/doc/hexagon/examples/config.example.json
	install -D -m 0644 deploy/proxy/compose.yaml $(DESTDIR)$(PREFIX)/share/doc/hexagon/examples/proxy/compose.yaml
	install -D -m 0644 deploy/proxy/nginx.conf $(DESTDIR)$(PREFIX)/share/doc/hexagon/examples/proxy/nginx.conf
	install -D -m 0644 deploy/proxy/README.md $(DESTDIR)$(PREFIX)/share/doc/hexagon/examples/proxy/README.md

# One container run: one static binary for one architecture, and the artifacts
# made from it. ARTIFACTS=deb skips the tarball. The frontend is built on the
# host first because web/node_modules is here and the Go image has no Node.
artifacts: build-web
	@mkdir -p $(HOME)/go/pkg/mod dist
	docker run --rm -u $$(id -u):$$(id -g) \
	  -v $(CURDIR):/src -w /src \
	  -v $(HOME)/go/pkg/mod:/go/pkg/mod \
	  -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOFLAGS=-buildvcs=false \
	  $(BUILD_IMAGE) packaging/deb/build.sh $(VERSION) $(ARCH) $(ARTIFACTS)

# One package for one architecture, for trying a change to the packaging out.
deb:
	$(MAKE) artifacts ARCH=$(ARCH) ARTIFACTS=deb

# Everything a GitHub release carries: a tarball and a package per architecture,
# and the checksums install.sh verifies against, written last so they cover
# every artifact beside them.
release:
	rm -rf dist
	@for a in $(RELEASE_ARCHES); do $(MAKE) artifacts ARCH=$$a ARTIFACTS=all || exit 1; done
	cd dist && sha256sum hexagon_* > SHA256SUMS
	@ls -l dist

clean:
	rm -rf $(BIN) $(WEB)/dist dist
	$(MAKE) dist-placeholder
