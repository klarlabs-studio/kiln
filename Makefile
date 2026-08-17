# Kiln — developer tasks.
GO      ?= go
BINDIR  ?= bin
DISTDIR ?= dist
MODULE   = go.klarlabs.de/kiln
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS ?= -X '$(MODULE)/internal/version.Version=$(VERSION)' -X '$(MODULE)/internal/version.Commit=$(COMMIT)' -X '$(MODULE)/internal/version.Date=$(DATE)'
PLATFORMS ?= linux/amd64 linux/arm64 darwin/arm64

IMAGE ?= ghcr.io/klarlabs-studio/kiln

.PHONY: all build install test race cover vet fmt fmt-check lint tidy dist dist-check examples-check docker release-check clean

all: fmt-check vet test build

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINDIR)/kiln  ./cmd/kiln
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINDIR)/kilnd ./cmd/kilnd

install:
	$(GO) install -ldflags "$(LDFLAGS)" ./cmd/kiln ./cmd/kilnd

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vet:
	$(GO) vet ./...

fmt:
	@git ls-files '*.go' | while IFS= read -r f; do [ ! -f "$$f" ] || printf '%s\0' "$$f"; done | xargs -0 gofmt -w

fmt-check:
	@out=$$(git ls-files '*.go' | while IFS= read -r f; do [ ! -f "$$f" ] || printf '%s\0' "$$f"; done | xargs -0 gofmt -l); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

lint:
	golangci-lint run

tidy:
	$(GO) mod tidy
	@git diff --exit-code go.mod go.sum

# examples-check keeps the shipped example honest: a pipeline in the README
# that no longer parses is worse than no example at all.
examples-check: build
	@set -e; for f in examples/*.yaml .kiln.yaml; do \
		echo "checking $$f"; \
		./$(BINDIR)/kiln doctor --config-only --pipeline "$$f" >/dev/null || exit 1; \
	done

dist:
	rm -rf $(DISTDIR)
	mkdir -p $(DISTDIR)
	@set -e; \
	for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		name=kiln_$(VERSION)_$${os}_$${arch}; \
		stage=$$(mktemp -d); \
		mkdir -p "$$stage/$$name/bin" "$$stage/$$name/docs" "$$stage/$$name/examples"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o "$$stage/$$name/bin/kiln"  ./cmd/kiln; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o "$$stage/$$name/bin/kilnd" ./cmd/kilnd; \
		cp README.md LICENSE "$$stage/$$name/"; \
		cp docs/*.md "$$stage/$$name/docs/"; \
		cp examples/*.yaml "$$stage/$$name/examples/"; \
		tar -C "$$stage" -czf "$(DISTDIR)/$$name.tar.gz" "$$name"; \
		rm -rf "$$stage"; \
	done
	cd $(DISTDIR) && shasum -a 256 *.tar.gz > checksums.txt

dist-check: dist
	test -s $(DISTDIR)/checksums.txt
	@count=$$(find $(DISTDIR) -name 'kiln_*.tar.gz' | wc -l | tr -d ' '); \
	want=$$(printf '%s\n' $(PLATFORMS) | wc -l | tr -d ' '); \
	if [ "$$count" != "$$want" ]; then echo "dist archive count $$count != $$want"; exit 1; fi
	cd $(DISTDIR) && shasum -a 256 -c checksums.txt

docker:
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

release-check: examples-check dist-check all

clean:
	rm -rf $(BINDIR) $(DISTDIR) coverage.out
