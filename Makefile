# dun — build/install with a version + source stamp.
#
# `make install` puts a LAUNCHER SCRIPT on your PATH (tools/dun.sh), not the
# binary: it rebuilds from this tree when anything changed and execs the result,
# so `dun` is always the latest build. The binary it manages lives in
# ~/.cache/dun (override with DUN_BIN); skip the check with DUN_NO_AUTOBUILD=1.
#
# `make build` produces ./dun the same way, stamped. `make install-bin` is the
# old behaviour — the stamped binary directly on PATH, self-updating from
# cmd/dun/selfupdate.go. A plain `go install ./cmd/dun` leaves srcDir empty and
# never updates at all (that's the release build).

SRCDIR  := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
VERSION := $(shell git -C $(SRCDIR) describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION) -X main.srcDir=$(SRCDIR)
GOBIN   := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN   := $(shell go env GOPATH)/bin
endif
# Where the launcher keeps the binary it builds. Must match tools/dun.sh.
DUN_BIN := $(if $(XDG_CACHE_HOME),$(XDG_CACHE_HOME),$(HOME)/.cache)/dun/dun

# Where `make web` installs the built client that dund serves (--web-root).
WEB_ROOT := $(if $(XDG_DATA_HOME),$(XDG_DATA_HOME),$(HOME)/.local/share)/dun/web

.PHONY: build install install-bin run test clean schema web

build: ## build ./dun (version + source stamped)
	go build -ldflags "$(LDFLAGS)" -o dun ./cmd/dun

install: ## put the self-rebuilding launcher on PATH (dun is always the latest)
	@mkdir -p $(dir $(DUN_BIN)) $(GOBIN)
	go build -ldflags "$(LDFLAGS)" -o $(DUN_BIN) ./cmd/dun
	@sed 's|__SRCDIR__|$(SRCDIR)|' tools/dun.sh > $(GOBIN)/dun.tmp
	@chmod +x $(GOBIN)/dun.tmp && mv -f $(GOBIN)/dun.tmp $(GOBIN)/dun
	@echo "installed $(GOBIN)/dun (launcher) → $(DUN_BIN) @ $(VERSION)"

install-bin: ## put the stamped BINARY on PATH instead (self-updates in-process)
	go install -ldflags "$(LDFLAGS)" ./cmd/dun
	@echo "installed $$(command -v dun) → $(VERSION)"

run: ## build + launch the TUI
	go run -ldflags "$(LDFLAGS)" ./cmd/dun -tui

test: ## build + vet + test
	go build ./... && go vet ./... && go test ./...

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
