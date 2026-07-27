BINARY := idlerthing
# Stamped into the binary as main.version. The release workflow overrides
# this with the tag; local builds report the git description, or "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run test vet notices notices-check license-headers license-check clean

# `notices` runs FIRST: THIRD_PARTY_LICENSES.md is embedded into the binary
# (notices.go), so regenerating it here means what ships always matches what
# is actually linked in — no separate step to forget after a dependency bump.
# The committed copy exists only so a bare `go build` still works.
build: notices
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) .

run:
	go run .

test:
	go test ./...

vet:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

# Regenerate the third-party notices from the modules actually linked in.
notices:
	@./scripts/gen-third-party-licenses.sh >/dev/null

# Fail if the committed notices are stale. Deliberately NOT part of the CI
# gate: Dependabot bumps go.mod but cannot regenerate this file, so gating
# every PR on it would make each gomod PR red for a reason the bot can't fix
# (the trap ircthing hit). The release workflow runs it instead — that is the
# point where the notice legally has to match the artifact being published.
notices-check:
	@tmp=$$(mktemp); \
	./scripts/gen-third-party-licenses.sh "$$tmp" >/dev/null; \
	if ! diff -q "$$tmp" THIRD_PARTY_LICENSES.md >/dev/null; then \
		echo "FAIL: THIRD_PARTY_LICENSES.md is stale — run 'make notices'"; \
		diff -u THIRD_PARTY_LICENSES.md "$$tmp" | head -40; \
		rm -f "$$tmp"; exit 1; \
	fi; \
	rm -f "$$tmp"; \
	echo "notices-check: THIRD_PARTY_LICENSES.md is current"

# Stamp the AGPL notice on any first-party source missing it.
license-headers:
	@./scripts/license-headers.sh

# Cheap and deterministic (no network, no build), so this one DOES gate CI.
license-check:
	@./scripts/license-headers.sh --check

clean:
	rm -f $(BINARY)
