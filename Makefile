GO ?= go
export CGO_ENABLED ?= 0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test test-race lint fmt vet cover run demo image clean

all: lint test

build:
	$(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/flagd ./cmd/flagd

# The race detector needs cgo, which this host's macOS SDK cannot link. CI runs
# test-race on Linux.
test:
	$(GO) test -count=1 ./...

test-race:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

cover:
	$(GO) test -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

lint: fmt vet

fmt:
	@unformatted=$$(gofmt -l . | grep -v '^$$' || true); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

vet:
	$(GO) vet ./...

## run: start flagd with the example flags
run:
	FLAGD_ADMIN_TOKEN=$${FLAGD_ADMIN_TOKEN:-dev-admin-token-at-least-16} \
	$(GO) run ./cmd/flagd -seed flags.example.json

## demo: start flagd and show an evaluation
demo: build
	@FLAGD_ADMIN_TOKEN=dev-admin-token-at-least-16 ./bin/flagd -seed flags.example.json & \
	sleep 2; \
	curl -s -X POST -H "Authorization: Bearer dev-admin-token-at-least-16" \
	  -d '{"flag_key":"new-checkout","context":{"key":"u1","attributes":{"email":"sam@acme.com"}}}' \
	  localhost:8080/api/v1/evaluate; echo; \
	kill %1

image:
	docker build --build-arg VERSION=$(VERSION) -t feature-flags:$(VERSION) .

clean:
	rm -rf bin coverage.out
