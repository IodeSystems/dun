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
# `go install ./cmd/dun` leaves srcDir empty and never updates at all (that's
# the release build).

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

.PHONY: build install install-launcher run test clean schema web ux

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

clean:
	rm -f ./dun
