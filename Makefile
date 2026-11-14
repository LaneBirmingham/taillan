# TailLAN build. `make help` lists targets.

# Use the Go version pinned in mise.toml when mise is installed.
GO      ?= $(if $(shell command -v mise 2>/dev/null),mise exec -- go,go)
TAGS    := $(shell cat build-tags.txt)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# Link included in the "Copy invite message" text, e.g. a releases page.
DOWNLOAD_URL ?=
MODULE  := $(shell $(GO) list -m)
LDFLAGS := -s -w -X main.version=$(VERSION) -X '$(MODULE)/internal/ui.DownloadURL=$(DOWNLOAD_URL)'
BUILD   := CGO_ENABLED=0 $(GO) build -trimpath -tags "$(TAGS)" -ldflags "$(LDFLAGS)"
PLATFORMS := linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64

.PHONY: build run test test-short vet fmt notices release clean help

build: ## Build ./taillan for this machine
	$(BUILD) -o taillan$(shell $(GO) env GOEXE) .

run: build ## Build and open the app
	./taillan

test: ## Unit tests + a real host/join round trip (needs internet; TAILLAN_SKIP_NET=1 to skip)
	$(GO) test -tags "$(TAGS)" -count=1 ./...

test-short: ## Offline tests only
	$(GO) test -tags "$(TAGS)" -short ./...

vet: ## go vet + gofmt check
	$(GO) vet -tags "$(TAGS)" ./...
	@out=$$("$$($(GO) env GOROOT)/bin/gofmt" -l .); \
	  if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

fmt: ## Format the code
	$(GO) fmt ./...

notices: ## Regenerate THIRD_PARTY_NOTICES.txt (after changing go.mod)
	GO="$(GO)" PLATFORMS="$(PLATFORMS)" sh scripts/notices.sh

release: clean ## Cross-compile for every platform into dist/
	mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  echo "  $$os/$$arch"; \
	  GOOS=$$os GOARCH=$$arch $(BUILD) -o dist/taillan-$$os-$$arch$$ext . || exit 1; \
	done
	cp LICENSE THIRD_PARTY_NOTICES.txt dist/
	cd dist && (sha256sum * 2>/dev/null || shasum -a 256 *) > SHA256SUMS
	@ls -lh dist

clean: ## Remove build output
	rm -rf dist taillan taillan.exe

help:
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'
