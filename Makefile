GO ?= go
BIN ?= bin/goldfinger

# Pinned in lockstep with .github/workflows/ci.yml and .golangci.yml. `make lint`
# runs it via `go run` (no separate install needed); CI uses the prebuilt action.
GOLANGCI_LINT_VERSION ?= v2.12.2

# Reproducible-build knobs (see the `repro` target). GOOS/GOARCH default to the
# host so `make repro` rebuilds the asset for this platform; override to verify
# another. The toolchain version is taken from go.mod's `go` directive — the
# single source CI's setup-go also keys off (go-version-file) — and forced via
# GOTOOLCHAIN so a newer local Go is stepped down to it, keeping the build
# deterministic without a separate pin to drift.
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
REPRO_TOOLCHAIN := go$(shell awk '/^go /{print $$2; exit}' go.mod)
REPRO_BIN = dist/goldfinger-$(GOOS)-$(GOARCH)
# sha256sum on Linux; fall back to `shasum -a 256` (macOS default) — same output.
SHA256SUM := $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo 'shasum -a 256')

.PHONY: help build test lint check repro e2e hooks clean

help:
	@echo "Targets:"
	@echo "  build   Build $(BIN)"
	@echo "  test    go vet + race tests (same commands as CI's test job)"
	@echo "  lint    golangci-lint (gosec + staticcheck), same config as CI's lint job"
	@echo "  check   build + test + lint"
	@echo "  repro   Rebuild a released tag reproducibly and print its sha256 (VERSION=vX.Y.Z)"
	@echo "  e2e     Full-pipeline e2e against the sandbox repo (needs GOLD_FINGER_PAT + gh)"
	@echo "  hooks   Install the gitleaks pre-commit hook into .git/hooks"
	@echo "  clean   Remove build artifacts"

# -mod=readonly on every build/test: refuse to mutate go.mod/go.sum, so a build
# fails loudly if a dependency's hash or version drifts from what's committed
# (module-hash verification) rather than silently rewriting the lockfiles.
build:
	$(GO) build -mod=readonly -o $(BIN) ./cmd

test:
	$(GO) vet -mod=readonly ./...
	$(GO) test -mod=readonly -race -count=2 ./...

lint:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

check: build test lint

# Reproducible-build verification: rebuild a released tag from source with the
# same toolchain and flags the release workflow uses, then print the sha256 so
# it can be compared against the published checksum. VERSION is required and
# must match the released tag exactly — it is baked into the binary via
# -ldflags -X main.version, and the embedded VCS stamp means you must run this
# from a CLEAN checkout of that tag's commit (see README "Verify a release").
# The build line also neutralises ambient Go env that would otherwise change the
# bytes and diverge from the release runner's defaults: GOFLAGS= (e.g. a stray
# -buildvcs=false would drop the VCS stamp), GOWORK=off (a go.work would swap
# deps), GOEXPERIMENT= (reset to the toolchain default set), GOAMD64=v1 (the
# release default; ignored off-amd64).
repro:
	@[ -n "$(VERSION)" ] || { echo 'VERSION is required and must match the released tag exactly, e.g. make repro VERSION=v0.5.0'; exit 1; }
	@mkdir -p dist
	GOFLAGS= GOWORK=off GOEXPERIMENT= GOAMD64=v1 \
		GOTOOLCHAIN=$(REPRO_TOOLCHAIN) CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o "$(REPRO_BIN)" ./cmd
	@echo
	@echo "Rebuilt $(REPRO_BIN)  (version $(VERSION), toolchain $(REPRO_TOOLCHAIN))"
	@cd dist && $(SHA256SUM) "goldfinger-$(GOOS)-$(GOARCH)"
	@echo "The line above matches goldfinger-$(GOOS)-$(GOARCH).sha256 from the $(VERSION) release (and its line in SHA256SUMS) hash-and-filename."

e2e:
	./scripts/e2e.sh

hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Installed .git/hooks/pre-commit"

clean:
	rm -rf bin dist
