.PHONY: all build clean test lint run

BINARY := ekilied
BUILD_DIR := build
VERSION := $(shell git describe --tags 2>/dev/null || echo "0.1.0")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")

build:
	GOOS=linux GOARCH=amd64 go build -ldflags="-X main.Version=$(VERSION) -X main.Commit=$(COMMIT)" -o $(BUILD_DIR)/$(BINARY) ./cmd/$(BINARY)

build-all:
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY)-linux-amd64 ./cmd/$(BINARY)
	GOOS=linux GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY)-linux-arm64 ./cmd/$(BINARY)

clean:
	rm -rf $(BUILD_DIR)/

test:
	go test -v -race -count=1 ./...

lint:
	go vet ./...

run:
	go run ./cmd/$(BINARY)

tidy:
	go mod tidy
