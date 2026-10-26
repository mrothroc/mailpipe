.PHONY: check fmt-check vet test build docs docs-check security secrets go-version-check vuln-check modules-check install-hooks install-security-tools

BIN := bin/mailpipe
GITLEAKS_VERSION := v8.30.1
GOVULNCHECK_VERSION := v1.9.0
export PATH := $(CURDIR)/bin:$(PATH)

check: fmt-check vet test build docs-check ## The release gate

fmt-check:
	@out=$$(gofmt -l cmd internal pkg tools); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

build: security
	go build -o $(BIN) ./cmd/mailpipe

security: secrets go-version-check modules-check vuln-check ## Online security gate

secrets:
	go run ./tools/security secrets

go-version-check:
	go run ./tools/security go-version

modules-check:
	go mod verify

vuln-check:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) -C cmd/mailpipe -scan=module

install-security-tools:
	GOBIN="$(CURDIR)/bin" go install -ldflags "-X github.com/zricethezav/gitleaks/v8/version.Version=$(GITLEAKS_VERSION)" github.com/zricethezav/gitleaks/v8@$(GITLEAKS_VERSION)

install-hooks:
	@current=$$(git config --get core.hooksPath || true); \
	if [ -n "$$current" ] && [ "$$current" != ".githooks" ]; then \
		echo "Existing core.hooksPath=$$current; integrate .githooks/pre-commit without replacing your hooks." >&2; exit 1; \
	fi; \
	if [ -z "$$current" ] && [ -f "$$(git rev-parse --git-path hooks/pre-commit)" ]; then \
		echo "An existing pre-commit hook is installed; integrate .githooks/pre-commit without replacing it." >&2; exit 1; \
	fi
	git config --local core.hooksPath .githooks

docs: ## Regenerate the generated regions in the docs
	go run ./tools/docgen sync

docs-check: ## Fail when a generated region no longer matches its source
	go run ./tools/docgen check
