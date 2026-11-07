GO ?= go
GOLANGCI_LINT_VERSION ?= v2.15.0
GOLANGCI_LINT ?= $(CURDIR)/bin/golangci-lint

.DEFAULT_GOAL := help

.PHONY: help tools fmt vet lint test test-race cover tidy example build-example up down check clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

tools: $(GOLANGCI_LINT) ## Install pinned dev tools into ./bin

$(GOLANGCI_LINT):
	GOBIN=$(CURDIR)/bin $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

fmt: ## Format the source
	$(GO) fmt ./...
	gofmt -l -w .

vet: ## Run go vet
	$(GO) vet ./...

lint: $(GOLANGCI_LINT) ## Run golangci-lint
	$(GOLANGCI_LINT) run

test: ## Run the tests
	$(GO) test ./...

test-race: ## Run the tests with the race detector
	$(GO) test -race ./...

cover: ## Run the tests and print total coverage
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

tidy: ## Tidy both modules
	$(GO) mod tidy
	cd example && $(GO) mod tidy

example: ## Run the example API
	cd example && $(GO) run .

build-example: ## Build the example binary into dist/
	cd example && $(GO) build -o ../dist/otelkit-example .

up: ## Start the example stack with Docker Compose
	cd example && docker compose up --build

down: ## Stop the example stack
	cd example && docker compose down

check: tidy fmt vet lint test test-race ## Run every check CI runs
	cd example && $(GO) vet ./... && $(GO) build ./...

clean: ## Remove build artifacts
	rm -rf coverage.out dist
