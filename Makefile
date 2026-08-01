GO ?= go
BIN ?= bin/goldfinger

.PHONY: help build test check e2e hooks clean

help:
	@echo "Targets:"
	@echo "  build   Build $(BIN)"
	@echo "  test    go vet + race tests (same commands as CI's test job)"
	@echo "  check   build + test"
	@echo "  e2e     Full-pipeline e2e against the sandbox repo (needs GOLD_FINGER_PAT + gh)"
	@echo "  hooks   Install the gitleaks pre-commit hook into .git/hooks"
	@echo "  clean   Remove build artifacts"

build:
	$(GO) build -o $(BIN) ./cmd

test:
	$(GO) vet ./...
	$(GO) test -race -count=2 ./...

check: build test

e2e:
	./scripts/e2e.sh

hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Installed .git/hooks/pre-commit"

clean:
	rm -rf bin dist
