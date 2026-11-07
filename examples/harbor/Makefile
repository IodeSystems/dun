# Static binaries the Harbor adapter uploads into each trial container.
MCPSHELL ?= ../../../mcpshell
CA ?= /etc/ssl/certs/ca-certificates.crt

.PHONY: bin
bin:
	mkdir -p bin
	cd ../.. && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o examples/harbor/bin/dun ./cmd/dun
	cd $(MCPSHELL) && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(CURDIR)/bin/mcpshell ./cmd/mcpshell
	cp $(CA) bin/ca.crt
