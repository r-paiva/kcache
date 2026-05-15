BINARY      := kcache
METRICS     := http://localhost:9090/metrics

# Pass extra flags to the binary, e.g. make run FLAGS="-stats 5s"
FLAGS    :=

.PHONY: all build generate test test-v fmt vet lint clean \
        run run-debug run-stats \
        trace metrics deps tidy

all: generate build


generate:
	go generate ./...

build:
	go build -o $(BINARY) .

test:
	go test ./internal/...

test-v:
	go test -v ./internal/...

test-race:
	go test -race ./internal/...

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

run: build
	sudo ./$(BINARY) $(FLAGS)

run-debug: build
	sudo ./$(BINARY) -log-level debug $(FLAGS)

run-stats: build
	sudo ./$(BINARY) -stats 5s $(FLAGS)

trace:
	sudo cat /sys/kernel/debug/tracing/trace_pipe

metrics:
	@curl -sf $(METRICS) | grep -E '^(kcache_|#)' || \
		echo "metrics endpoint not reachable — is kcache running?"

metrics-watch:
	watch -n2 "curl -sf $(METRICS) | grep -E '^kcache_' | grep -v '^#'"

deps:
	sudo apt-get install -y clang llvm libbpf-dev linux-libc-dev linux-headers-$$(uname -r)

clean:
	rm -f $(BINARY)
	rm -f kcache_bpfel.go kcache_bpfeb.go kcache_bpfel.o kcache_bpfeb.o
