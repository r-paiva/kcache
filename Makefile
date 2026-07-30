# SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
#
# SPDX-License-Identifier: Apache-2.0

BINARY      := latch
METRICS     := http://localhost:9090/metrics
FLAGS       :=

DOCKER_USER   ?= rpaiva0
IMAGE_NAME    ?= latch
IMAGE_TAG     ?= dev
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build generate test test-v test-race cover cover-html fmt vet lint tidy clean \
        run run-debug run-stats trace metrics metrics-watch deps \
        image image-push reuse-lint reuse-fix help

# ── Build ─────────────────────────────────────────────────────────────────────

all: build

generate:
	go generate ./...

build: generate
	go build -ldflags "-X codeberg.org/latch/latch/internal/version.Version=$(VERSION)" -o $(BINARY) .

test:
	go test ./internal/...

test-v:
	go test -v ./internal/...

test-race:
	go test -race ./internal/...

cover:
	go test -coverprofile=coverage.out -covermode=atomic ./internal/...
	go tool cover -func=coverage.out

cover-html: cover
	go tool cover -html=coverage.out

fmt:
	gofmt -w -s .

vet:
	go vet ./...

lint:
	@command -v golangci-lint >/dev/null 2>&1 || \
		{ echo "golangci-lint not found — install from https://golangci-lint.run/usage/install/"; exit 1; }
	golangci-lint run ./...

tidy:
	go mod tidy

reuse-lint:
	reuse lint

reuse-fix:
	reuse annotate --license Apache-2.0 \
	  --copyright "Copyright (c) 2026, the latch developers"

deps:
	sudo apt-get install -y clang llvm libbpf-dev linux-libc-dev linux-headers-$$(uname -r)

clean:
	rm -f $(BINARY)
	rm -f latch_bpfel.go latch_bpfeb.go latch_bpfel.o latch_bpfeb.o
	rm -f coverage.out coverage.html

# ── Local run ─────────────────────────────────────────────────────────────────

run: build
	sudo ./$(BINARY) $(FLAGS)

run-debug: build
	sudo ./$(BINARY) -log-level debug $(FLAGS)

run-stats: build
	sudo ./$(BINARY) -stats 5s $(FLAGS)

trace:
	sudo cat /sys/kernel/debug/tracing/trace_pipe

metrics:
	@curl -sf $(METRICS) | grep -E '^(latch_|#)' || \
		echo "metrics endpoint not reachable — is latch running?"

metrics-watch:
	watch -n2 "curl -sf $(METRICS) | grep -E '^latch_' | grep -v '^#'"

# ── Image ─────────────────────────────────────────────────────────────────────

image:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  -t $(IMAGE_NAME):$(IMAGE_TAG) .

image-push:
	docker buildx build --platform linux/amd64,linux/arm64 \
	  --build-arg VERSION=$(VERSION) \
	  -t $(DOCKER_USER)/$(IMAGE_NAME):$(IMAGE_TAG) \
	  --push \
	  .

# ── Help ──────────────────────────────────────────────────────────────────────

help:
	@echo "Build:"
	@echo "  make / make build       generate BPF objects + build binary"
	@echo "  make generate           regenerate BPF Go bindings"
	@echo "  make test               unit tests"
	@echo "  make cover              coverage report"
	@echo "  make lint               golangci-lint"
	@echo ""
	@echo "Run locally (requires root for BPF):"
	@echo "  make run                build + run"
	@echo "  make run-debug          build + run with debug logging"
	@echo "  make trace              tail BPF kernel trace pipe"
	@echo ""
	@echo "Image (DOCKER_USER=$(DOCKER_USER) IMAGE_TAG=$(IMAGE_TAG)):"
	@echo "  make image              local docker build"
	@echo "  make image-push         multi-arch push to Docker Hub"
	@echo ""
	@echo "Charts: latch-charts repo   Deploy/testing: latch-homelab repo"
