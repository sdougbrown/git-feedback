.PHONY: build test-bin test test-race vet fmtcheck install verify registry-test

build:
	go build -o ./bin/git-feedback ./cmd/git-feedback

test-bin:
	go build -tags testhooks -o ./bin/git-feedback-test ./cmd/git-feedback

test: build test-bin
	go test ./...

test-race: build test-bin
	go test -race ./...

vet:
	go vet ./...

fmtcheck:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

install:
	go install ./cmd/git-feedback

verify: fmtcheck vet test test-race build

# registry-test is the deliberate way to run the cli-registry smoke gate
# (integration/registry_test.go). It is kept out of verify because it depends
# on an external repo (~/Code/cli-registry) or a prebuilt CLI_REGISTRY_BIN.
registry-test: build test-bin
	@tmp=""; \
	if [ -z "$$CLI_REGISTRY_BIN" ]; then \
		tmp=$$(mktemp -d); \
		(cd $$HOME/Code/cli-registry && go build -o "$$tmp/cli-registry" ./cmd/cli-registry) || { echo "error: failed to build cli-registry from $$HOME/Code/cli-registry" >&2; rm -rf "$$tmp"; exit 1; }; \
		CLI_REGISTRY_BIN="$$tmp/cli-registry"; \
	fi; \
		echo "using CLI_REGISTRY_BIN=$$CLI_REGISTRY_BIN"; \
	CLI_REGISTRY_BIN="$$CLI_REGISTRY_BIN" go test ./integration -run 'TestManifest' -count=1; \
	status=$$?; \
	if [ -n "$$tmp" ]; then rm -rf "$$tmp"; fi; \
	exit $$status
