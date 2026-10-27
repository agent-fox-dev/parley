.DEFAULT_GOAL := help
.PHONY: help build test test-smoke fmt fmt-check vet lint check check-release tidy clean

NPM ?= npm

help: ## List all targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Type-check and bundle the worker
	$(NPM) run build

test: ## Run the fast test suite
	$(NPM) run test

test-smoke: ## Run the smoke suites against a running deployment
	$(NPM) run test:smoke

fmt: ## Format all source files in place
	$(NPM) run fmt

fmt-check: ## Fail if any source file is unformatted
	$(NPM) run fmt:check

vet: ## Statically check the sources
	$(NPM) run vet

lint: ## Lint the sources
	$(NPM) run lint

check: fmt-check vet lint test ## The fast gate: run before committing

check-release: fmt-check vet lint test test-smoke ## The full gate; run by a person, never by an agent

tidy: ## Reconcile the dependency manifest and lockfile
	$(NPM) install --package-lock-only

clean: ## Remove build output
	rm -rf dist .wrangler
