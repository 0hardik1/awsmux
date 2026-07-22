BINARY := ./bin/awsmux
# Keep in sync with .github/workflows/ci.yml (golangci-lint-action "version").
GOLANGCI_LINT_VERSION := v2.12.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
# Fleet size: teams x {prod,stage} x shards, plus one deliberate duplicate
# profile. Defaults produce the canonical 101-profile fleet.
FLEET_TEAMS ?= 10
FLEET_SHARDS ?= 5

.PHONY: setup build test vet fmt check-fmt lint e2e fleet-up fleet-down \
        install-hooks uninstall-hooks clean

# One-time dev setup: git hooks plus a warm build of the pinned lint tool
# (the first `go run` compiles it; afterwards it is build-cached).
setup: install-hooks
	go mod download
	$(GOLANGCI_LINT) --version >/dev/null

build:
	@mkdir -p bin
	go build -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

# Rewrite files in place with gofmt.
fmt:
	gofmt -l -w .

# Fail if any file needs gofmt (CI uses this).
check-fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then \
	  echo "gofmt needed on:"; echo "$$out"; exit 1; fi

lint:
	$(GOLANGCI_LINT) run

# Boot LocalStack and provision the test fleet under .tmp/fleet/.
# Then: source .tmp/fleet/env.sh && ./bin/awsmux targets
fleet-up:
	go run ./scripts/fleet up -teams $(FLEET_TEAMS) -shards $(FLEET_SHARDS)

fleet-down:
	go run ./scripts/fleet down

# End-to-end smoke test against the fleet (needs Docker and the aws CLI).
e2e: build fleet-up
	./scripts/e2e.sh

install-hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/pre-commit .githooks/commit-msg
	@echo "Hooks installed. Bypass per-commit with: git commit --no-verify"

uninstall-hooks:
	-git config --unset core.hooksPath

clean:
	rm -rf bin .tmp
