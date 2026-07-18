# dun — build/install with a version + source stamp.
#
# `make install` puts a source-stamped dun on your PATH; from then on it
# self-updates on launch when the tree changes (see cmd/dun/selfupdate.go).
# `make build` produces ./dun the same way. A plain `go install ./cmd/dun`
# leaves srcDir empty → no self-update (that's the release build).

SRCDIR  := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
VERSION := $(shell git -C $(SRCDIR) describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION) -X main.srcDir=$(SRCDIR)

.PHONY: build install run test clean

build: ## build ./dun (version + source stamped)
	go build -ldflags "$(LDFLAGS)" -o dun ./cmd/dun

install: ## reinstall dun on PATH; it then self-updates on launch
	go install -ldflags "$(LDFLAGS)" ./cmd/dun
	@echo "installed $$(command -v dun) → $(VERSION)"

run: ## build + launch the TUI
	go run -ldflags "$(LDFLAGS)" ./cmd/dun -tui

test: ## build + vet + test
	go build ./... && go vet ./... && go test ./...

clean:
	rm -f ./dun
