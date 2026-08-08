GO ?= go
BIN ?= bin/goldfinger

# Pinned in lockstep with .github/workflows/ci.yml and .golangci.yml. `make lint`
# runs it via `go run` (no separate install needed); CI uses the prebuilt action.
GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: help build test lint check e2e hooks clean

help:
	@echo "Targets:"
	@echo "  build   Build $(BIN)"
	@echo "  test    go vet + race tests (same commands as CI's test job)"
	@echo "  lint    golangci-lint (gosec + staticcheck), same config as CI's lint job"
	@echo "  check   build + test + lint"
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

e2e:
	./scripts/e2e.sh

hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Installed .git/hooks/pre-commit"

clean:
	rm -rf bin dist
