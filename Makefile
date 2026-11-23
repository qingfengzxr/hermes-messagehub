GO ?= go
VERSION ?= dev

.PHONY: test race vet fmt check build image

test:
	$(GO) test ./... -count=1

race:
	$(GO) test -race ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

check: test race vet

build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -mod=readonly -trimpath -ldflags '-s -w -X main.version=$(VERSION)' -o dist/messagehub ./cmd/messagehub

image:
	docker build -t messagehub:local .
