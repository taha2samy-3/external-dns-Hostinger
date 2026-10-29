GO ?= go
IMAGE_NAME ?= ghcr.io/taha2samy-3/external-dns-hostinger-webhook
IMAGE_TAG ?= dev
OUT := $(CURDIR)/_out

.PHONY: all
all: lint test

.PHONY: build
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w' -o '$(OUT)/webhook' ./cmd/webhook

.PHONY: test
test:
	$(GO) test -race -count=1 ./...

.PHONY: lint
lint:
	$(GO) vet ./...
	golangci-lint run ./...

.PHONY: image
image:
	docker build --build-arg VERSION=$(IMAGE_TAG) -t $(IMAGE_NAME):$(IMAGE_TAG) .
