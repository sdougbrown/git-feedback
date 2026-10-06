.PHONY: build test-bin test test-race vet fmtcheck install verify

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
