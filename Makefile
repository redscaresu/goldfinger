GO ?= go
BIN ?= bin/goldfinger

.PHONY: help build test check hooks clean

help:
	@echo "Targets:"
	@echo "  build   Build $(BIN)"
	@echo "  test    go vet + race tests (same commands as CI's test job)"
	@echo "  check   build + test"
	@echo "  hooks   Install the gitleaks pre-commit hook into .git/hooks"
	@echo "  clean   Remove build artifacts"

build:
	$(GO) build -o $(BIN) ./cmd

test:
	$(GO) vet ./...
	$(GO) test -race -count=2 ./...

check: build test

hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "Installed .git/hooks/pre-commit"

clean:
	rm -rf bin dist
