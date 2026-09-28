SHELL := /bin/sh

GO ?= go
BINARY ?= bin/opora
COVERPROFILE ?= coverage.out
COVERAGE_THRESHOLD ?= 90.0
CORE_PACKAGES := ./internal/policy ./internal/validation ./internal/service

.PHONY: build run test test-race vet coverage coverage-check check lint clean

build:
	mkdir -p "$(dir $(BINARY))"
	$(GO) build -trimpath -o "$(BINARY)" ./cmd/service

run:
	$(GO) run ./cmd/service

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

coverage:
	$(GO) test -covermode=atomic -coverprofile="$(COVERPROFILE)" $(CORE_PACKAGES)
	$(GO) tool cover -func="$(COVERPROFILE)"

coverage-check: coverage
	@total="$$( $(GO) tool cover -func="$(COVERPROFILE)" | awk '/^total:/ { gsub(/%/, "", $$3); print $$3 }' )"; \
	if [ -z "$$total" ]; then \
		echo "coverage total is unavailable" >&2; \
		exit 1; \
	fi; \
	awk -v total="$$total" -v threshold="$(COVERAGE_THRESHOLD)" 'BEGIN { \
		if ((total + 0) < (threshold + 0)) { \
			printf "core statement coverage %.1f%% is below %.1f%%\n", total, threshold > "/dev/stderr"; \
			exit 1; \
		} \
		printf "core statement coverage %.1f%% meets %.1f%%\n", total, threshold; \
	}'

check:
	$(MAKE) vet
	$(MAKE) test
	$(MAKE) test-race
	$(MAKE) coverage-check

lint:
	golangci-lint run ./...

clean:
	rm -f "$(BINARY)" "$(COVERPROFILE)" coverage.html
