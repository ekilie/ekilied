.PHONY: all build build-all build-linux build-darwin clean test lint run \
        deps release checksums

# ── Variables ────────────────────────────────────────────────────────────────
APP_NAME    := ekilied
MODULE      := github.com/ekilie/ekilied
LATEST_TAG  := $(shell git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | head -1)
NEXT_PATCH   = $(shell echo $(LATEST_TAG) | awk -F. '{printf "%s.%s.%d", $$1, $$2, $$3+1}')
VERSION     ?= $(if $(LATEST_TAG),$(NEXT_PATCH),v0.1.0)
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
BUILD_DATE  := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS     := -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.Commit=$(COMMIT)'
BUILD_FLAGS := -trimpath
BUILD_DIR   := ./build
CGO_ENABLED ?= 0

# ── Development ───────────────────────────────────────────────────────────────
build:
	CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME) ./cmd/$(APP_NAME)

run: build
	./$(BUILD_DIR)/$(APP_NAME)

dev:
	go run ./cmd/$(APP_NAME)

deps:
	go mod tidy

test:
	go test -v -race -count=1 ./...

lint:
	go vet ./...

clean:
	rm -rf $(BUILD_DIR)/

# ── Cross-platform builds ────────────────────────────────────────────────────
build-all: deps
	@echo "==> Building for all platforms ($(VERSION))..."
	mkdir -p $(BUILD_DIR)
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-amd64   ./cmd/$(APP_NAME)
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-arm64   ./cmd/$(APP_NAME)
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-darwin-amd64  ./cmd/$(APP_NAME)
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-darwin-arm64  ./cmd/$(APP_NAME)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-windows-amd64.exe ./cmd/$(APP_NAME)
	@echo "==> Packaging archives..."
	cd $(BUILD_DIR) && cp $(APP_NAME)-linux-amd64  $(APP_NAME) && tar czf $(APP_NAME)-linux-amd64.tar.gz  $(APP_NAME) && rm $(APP_NAME)
	cd $(BUILD_DIR) && cp $(APP_NAME)-linux-arm64  $(APP_NAME) && tar czf $(APP_NAME)-linux-arm64.tar.gz  $(APP_NAME) && rm $(APP_NAME)
	cd $(BUILD_DIR) && cp $(APP_NAME)-darwin-amd64 $(APP_NAME) && tar czf $(APP_NAME)-darwin-amd64.tar.gz $(APP_NAME) && rm $(APP_NAME)
	cd $(BUILD_DIR) && cp $(APP_NAME)-darwin-arm64 $(APP_NAME) && tar czf $(APP_NAME)-darwin-arm64.tar.gz $(APP_NAME) && rm $(APP_NAME)
	cd $(BUILD_DIR) && cp $(APP_NAME)-windows-amd64.exe $(APP_NAME).exe && zip $(APP_NAME)-windows-amd64.zip $(APP_NAME).exe && rm $(APP_NAME).exe
	@echo "==> Done! Binaries in $(BUILD_DIR)/"

build-linux: deps
	@echo "==> Building for Linux..."
	mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-amd64 ./cmd/$(APP_NAME)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-arm64 ./cmd/$(APP_NAME)
	cd $(BUILD_DIR) && cp $(APP_NAME)-linux-amd64 $(APP_NAME) && tar czf $(APP_NAME)-linux-amd64.tar.gz $(APP_NAME) && rm $(APP_NAME)
	cd $(BUILD_DIR) && cp $(APP_NAME)-linux-arm64 $(APP_NAME) && tar czf $(APP_NAME)-linux-arm64.tar.gz $(APP_NAME) && rm $(APP_NAME)

build-darwin: deps
	@echo "==> Building for macOS..."
	mkdir -p $(BUILD_DIR)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-darwin-amd64 ./cmd/$(APP_NAME)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=$(CGO_ENABLED) go build $(BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-darwin-arm64 ./cmd/$(APP_NAME)
	cd $(BUILD_DIR) && cp $(APP_NAME)-darwin-amd64 $(APP_NAME) && tar czf $(APP_NAME)-darwin-amd64.tar.gz $(APP_NAME) && rm $(APP_NAME)
	cd $(BUILD_DIR) && cp $(APP_NAME)-darwin-arm64 $(APP_NAME) && tar czf $(APP_NAME)-darwin-arm64.tar.gz $(APP_NAME) && rm $(APP_NAME)

# ── SHA256 checksums ─────────────────────────────────────────────────────────
checksums:
	@echo "==> Generating checksums..."
	cd $(BUILD_DIR) && shasum -a 256 *.tar.gz *.zip > checksums.txt
	@cat $(BUILD_DIR)/checksums.txt

# ── GitHub Release ───────────────────────────────────────────────────────────
# Usage:
#   make release                              # auto-bump patch version
#   make release VERSION=v0.2.0               # specific version
#   make release VERSION=v0.2.0 FORCE=1       # overwrite existing

release: clean build-all checksums
	@echo "==> Tagging $(VERSION)..."
	git tag -f $(VERSION)
	git push origin $(VERSION) --force
ifdef FORCE
	@echo "==> Deleting existing release $(VERSION) (FORCE=1)..."
	-gh release delete $(VERSION) --yes --cleanup-tag 2>/dev/null || true
	@sleep 2
endif
	@echo "==> Creating GitHub release $(VERSION)..."
	gh release create $(VERSION) \
		--title "Ekilied $(VERSION)" \
		--generate-notes \
		$(BUILD_DIR)/*.tar.gz \
		$(BUILD_DIR)/*.zip \
		$(BUILD_DIR)/checksums.txt
