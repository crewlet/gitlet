GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/crewlet/gitlet/internal/cli.Version=$(VERSION) \
	-X github.com/crewlet/gitlet/internal/cli.Commit=$(COMMIT) \
	-X github.com/crewlet/gitlet/internal/cli.Date=$(DATE)

.PHONY: all build test race vet lint fmt tidy clean install

all: vet test build

build:
	CGO_ENABLED=1 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/gitlet ./cmd/gitlet

install:
	CGO_ENABLED=1 $(GO) install -trimpath -ldflags "$(LDFLAGS)" ./cmd/gitlet

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

fmt:
	gofmt -w .

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin dist
