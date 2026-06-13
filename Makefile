SHELL := /bin/bash

MODULE    := github.com/byjackchen/trade-tms-go
BIN_DIR   := bin
BINARY    := $(BIN_DIR)/tms

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(MODULE)/internal/app.Version=$(VERSION) \
	-X $(MODULE)/internal/app.Commit=$(COMMIT) \
	-X $(MODULE)/internal/app.BuildDate=$(BUILD_DATE)

# Integration tests run against the compose stack (host-mapped ports).
ITEST_ENV := TMS_PG_HOST=127.0.0.1 TMS_PG_PORT=55432 TMS_PG_USER=tms \
	TMS_PG_PASSWORD=tms TMS_PG_DATABASE=tms TMS_REDIS_ADDR=127.0.0.1:56379

.PHONY: all build test vet fmt-check itest compose-up compose-down docker-build clean

all: fmt-check vet build test

build:
	mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/tms

test:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt: the following files need formatting:"; echo "$$out"; exit 1; \
	fi

# Bring up the base stack (postgres + redis + migrations) and run
# integration-tagged tests against it.
itest: compose-up
	$(ITEST_ENV) go test -race -tags integration -count=1 ./...

compose-up:
	docker compose up -d --build --wait postgres redis migrate

compose-down:
	docker compose down

docker-build:
	docker build -t tms:dev \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .

clean:
	rm -rf $(BIN_DIR)
