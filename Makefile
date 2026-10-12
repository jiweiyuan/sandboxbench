BIN     := bin/sandboxbench
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test vet fmt lint smoke paper clean

all: build

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/sandboxbench

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Everything CI checks.
lint: fmt vet test

# Run the whole suite against the local adapter. Needs no credentials.
smoke: build
	$(BIN) run --config configs/v01-local.yaml --out results

paper:
	$(MAKE) -C paper

clean:
	rm -rf bin
	$(MAKE) -C paper clean
