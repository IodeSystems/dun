# dun — build/install with a version + source stamp.
#
# `make install` builds the stamped binary into ~/.local/bin/dun (override with
# BINDIR=…). It self-updates from this tree (cmd/dun/selfupdate.go).
#
# `make install-launcher` puts a LAUNCHER SCRIPT on your PATH instead
# (tools/dun.sh): it rebuilds from this tree when anything changed and execs
# the result. The binary it manages lives in ~/.cache/dun (override with
# DUN_BIN); skip the check with DUN_NO_AUTOBUILD=1.
#
# `make build` produces ./dun the same way, stamped. A plain
# `go install ./cmd/dun` leaves srcDir empty and never updates itself.
#
# `make release` builds the release builds (-tags release, -X main.channel=
# release): they carry the manifest URL and the minisign key, install tools
# from the pinned release/deps.json, and update from signed releases. See
# the release target below and plan item 47.

SRCDIR  := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
VERSION := $(shell git -C $(SRCDIR) describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION) -X main.srcDir=$(SRCDIR)
GOBIN   := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN   := $(shell go env GOPATH)/bin
endif
# Where `make install` puts the binary.
BINDIR  := $(HOME)/.local/bin
# Where the launcher keeps the binary it builds. Must match tools/dun.sh.
DUN_BIN := $(if $(XDG_CACHE_HOME),$(XDG_CACHE_HOME),$(HOME)/.cache)/dun/dun

# Where `make web` installs the built client that dund serves (--web-root).
WEB_ROOT := $(if $(XDG_DATA_HOME),$(XDG_DATA_HOME),$(HOME)/.local/share)/dun/web

.PHONY: build install install-launcher run test clean schema web ux release site site-push

build: ## build ./dun (version + source stamped)
	go build -ldflags "$(LDFLAGS)" -o dun ./cmd/dun

install: ## build the stamped binary into ~/.local/bin/dun (BINDIR=… to change)
	@mkdir -p $(BINDIR)
	go build -ldflags "$(LDFLAGS)" -o $(BINDIR)/dun.tmp ./cmd/dun
	@mv -f $(BINDIR)/dun.tmp $(BINDIR)/dun
	@echo "installed $(BINDIR)/dun @ $(VERSION)"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "note: $(BINDIR) is not on PATH";; esac

install-launcher: ## put the self-rebuilding launcher on PATH (dun is always the latest)
	@mkdir -p $(dir $(DUN_BIN)) $(GOBIN)
	go build -ldflags "$(LDFLAGS)" -o $(DUN_BIN) ./cmd/dun
	@sed 's|__SRCDIR__|$(SRCDIR)|' tools/dun.sh > $(GOBIN)/dun.tmp
	@chmod +x $(GOBIN)/dun.tmp && mv -f $(GOBIN)/dun.tmp $(GOBIN)/dun
	@echo "installed $(GOBIN)/dun (launcher) → $(DUN_BIN) @ $(VERSION)"

run: ## build + launch the TUI
	go run -ldflags "$(LDFLAGS)" ./cmd/dun -tui

test: ## build + vet (Linux and macOS, CLI-14) + test, and the recorder (its own module)
	go build ./... && go vet ./... && GOOS=darwin go vet ./... && go test ./...
	cd tools/ttyrec && go vet ./... && go test ./...

# A UX walk: hermetic dun + stub LLM, recorded at each of the walk's sizes;
# writes tmp/ux-run-<walk>-<stamp>-<cols>x<rows>/ per size
# for the lenny / george / ives agents. Not part of `test` (about a minute a size).
WALK ?= onboarding
ux: ## walk dun as a new user (WALK=onboarding) → tmp/ux-run-*/
	bin/ux $(WALK)

schema: ## regenerate mobile/schema.graphql from dund's operations
	go run ./cmd/dun dund schema > mobile/schema.graphql

web: ## build the client (mobile/, react-native-web) and install it for dund's --web-root
	cd mobile && pnpm install --frozen-lockfile && pnpm export:web
	@rm -rf $(WEB_ROOT).new && mkdir -p $(dir $(WEB_ROOT)) && cp -r mobile/dist $(WEB_ROOT).new
	@if [ -d $(WEB_ROOT) ]; then mv $(WEB_ROOT) $(WEB_ROOT).old; fi
	@mv $(WEB_ROOT).new $(WEB_ROOT) && rm -rf $(WEB_ROOT).old
	@echo "installed the web client → $(WEB_ROOT)"

# Release builds, for local use: one tar.gz per platform, dun-manifest.json
# (tools/mkmanifest: dun's entries from these archives, the tools' from
# release/deps.json) and, when MINISIGN_KEY names a minisign secret key and
# the minisign CLI is here, its signature. Publishing (a tag, a GitHub
# release, CI) is the owner's: nothing here uploads anything.
#
# release/deps.json is filled by hand from mcpshell's and poly-lsp-mcp's own
# releases: per tool {"version": "vX.Y.Z", "files": {"linux_amd64": {"url",
# "sha256", "size"}, …}}, the sha256 and size of each tool's
# <name>_<os>_<arch>.tar.gz as published (sha256sum, stat -c %s). The release
# builds embed it, so it is filled BEFORE make release. Each version must be
# the tool's Recommended in release/supported.go (every build's table; a
# test checks the two agree), so set that first.
RELEASE_VERSION ?= $(VERSION)
RELEASE_DIR     := dist/$(RELEASE_VERSION)
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
RELEASE_URL     := https://github.com/IodeSystems/dun/releases/download/$(RELEASE_VERSION)
RELEASE_LDFLAGS := -s -w -X main.version=$(RELEASE_VERSION) -X main.channel=release
RELEASE_SUMMARY ?=
MINISIGN_KEY    ?=

release: ## release builds + signed dun-manifest.json → dist/<version>/ (RELEASE_VERSION=vX.Y.Z, MINISIGN_KEY=…)
	@case "$(RELEASE_VERSION)" in v[0-9]*.[0-9]*.[0-9]*-*|*-dirty|*-g*) echo "make release: $(RELEASE_VERSION) is not vX.Y.Z (tag a clean commit, or RELEASE_VERSION=vX.Y.Z)"; exit 1;; v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "make release: $(RELEASE_VERSION) is not vX.Y.Z (tag a clean commit, or RELEASE_VERSION=vX.Y.Z)"; exit 1;; esac
	@if grep -q PLACEHOLDER release/minisign.pub; then echo; echo "WARNING: release/minisign.pub is the PLACEHOLDER: these builds can never update (aw4 issue i_1m5r9bx5a53x000taq6xar)"; echo; fi
	rm -rf $(RELEASE_DIR) && mkdir -p $(RELEASE_DIR)
	@for t in $(RELEASE_TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; d=$(RELEASE_DIR)/dun_$${os}_$${arch}; \
		echo "build $$os/$$arch"; mkdir -p $$d || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -tags release -ldflags "$(RELEASE_LDFLAGS)" -o $$d/dun ./cmd/dun || exit 1; \
		tar -C $$d -czf $(RELEASE_DIR)/dun_$${os}_$${arch}.tar.gz dun || exit 1; rm -rf $$d; \
	done
	go run ./tools/mkmanifest -version $(RELEASE_VERSION) -dist $(RELEASE_DIR) -url $(RELEASE_URL) \
		-summary "$(RELEASE_SUMMARY)" -o $(RELEASE_DIR)/dun-manifest.json
	@if [ -n "$(MINISIGN_KEY)" ] && command -v minisign >/dev/null 2>&1; then \
		minisign -S -s "$(MINISIGN_KEY)" -m $(RELEASE_DIR)/dun-manifest.json -t "dun $(RELEASE_VERSION) manifest" && \
		echo "signed $(RELEASE_DIR)/dun-manifest.json.minisig"; \
	else \
		echo; echo "WARNING: dun-manifest.json is NOT SIGNED (set MINISIGN_KEY and install minisign): no release build will accept it"; echo; \
	fi
	@ls -l $(RELEASE_DIR)

# dun.iodesystems.com (docs/registry.md), served by hz as a static service:
# the page and the registry index, signed with the release key. hz's API is
# on the home network, so the push runs here, not in CI. The entries'
# sources are not published, only the index.
SITE_DIR := dist/site

site: ## the site and the signed registry → dist/site (MINISIGN_KEY=…, minisign on PATH)
	@test -n "$(MINISIGN_KEY)" || { echo "make site: MINISIGN_KEY names the release key's secret file"; exit 1; }
	rm -rf $(SITE_DIR) && mkdir -p $(SITE_DIR)
	cp -r site/. $(SITE_DIR)/
	python3 tools/promptsize/benchpage.py $(SITE_DIR)/bench
	rm -rf $(SITE_DIR)/registry/v1/extensions $(SITE_DIR)/registry/v1/presets $(SITE_DIR)/registry/v1/tools
	go run ./tools/mkregistry -src site/registry/v1 -root . -o $(SITE_DIR)/registry/v1/index.json
	minisign -S -s "$(MINISIGN_KEY)" -m $(SITE_DIR)/registry/v1/index.json -t "dun registry"
	minisign -V -p release/minisign.pub -m $(SITE_DIR)/registry/v1/index.json

site-push: site ## build, sign and push dist/site to hz (HZ_URL, HZ_TOKEN: the site service's; hz-client on PATH)
	@test -n "$$HZ_URL" && test -n "$$HZ_TOKEN" || { echo "make site-push: HZ_URL and HZ_TOKEN (the site service's Integration panel in hz)"; exit 1; }
	hz-client site push $(SITE_DIR)

clean:
	rm -f ./dun
