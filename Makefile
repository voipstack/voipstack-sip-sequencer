# Makefile — single repeatable build/release entry point for voipstack-sip-sequencer.
# Deterministic static builds: CGO_ENABLED=0 + -trimpath + stable ldflags.
# Reproducibility also requires the pinned Go toolchain (see the `go` directive in go.mod).

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

GO      ?= go
GOOS    ?= linux
GOARCH  ?= amd64

BIN := sip-sequencer
PKG := ./cmd/sip-sequencer
DIST := dist

LDFLAGS := -s -w -X main.version=$(VERSION)

ARTIFACT := $(DIST)/$(BIN)-$(VERSION)-$(GOOS)-$(GOARCH)

.PHONY: build release checksum deb test clean

build: | $(DIST)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN) $(PKG)

release: | $(DIST)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(ARTIFACT) $(PKG)
	$(MAKE) checksum
	@echo "release artifact: $(ARTIFACT)"

checksum:
	cd $(DIST) && sha256sum $(BIN)-$(VERSION)-$(GOOS)-$(GOARCH) > $(BIN)-$(VERSION)-$(GOOS)-$(GOARCH).sha256

# Build a Debian .deb via nfpm (no debhelper toolchain). The leading `v` of a tag is
# stripped for the Debian version (v1.2.3 -> 1.2.3); `dev` is passed through. VERSION is
# exported so packaging/nfpm.yaml picks it up via ${VERSION}. Requires nfpm on PATH.
deb: build
	VERSION=$(VERSION:v%=%) nfpm package -p deb -f packaging/nfpm.yaml --target $(DIST)/
	@echo "deb artifact in: $(DIST)/"

test:
	$(GO) test -race ./...

clean:
	rm -rf $(DIST)

$(DIST):
	mkdir -p $(DIST)
