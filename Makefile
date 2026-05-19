BIN := bin/hospital-sidecar
PKG := ./cmd/hospital-agent

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-all build-linux build-linux-arm64 build-darwin build-darwin-arm64 run clean fmt vet

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

build-all: build-linux build-linux-arm64 build-darwin build-darwin-arm64

build-linux:
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/hospital-sidecar-linux-amd64 $(PKG)

build-linux-arm64:
	@mkdir -p bin
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/hospital-sidecar-linux-arm64 $(PKG)

build-darwin:
	@mkdir -p bin
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/hospital-sidecar-darwin-amd64 $(PKG)

build-darwin-arm64:
	@mkdir -p bin
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/hospital-sidecar-darwin-arm64 $(PKG)

run: build
	HOSPITAL_AGENT_INBOUND_TOKEN=devtoken \
	HOSPITAL_AGENT_API_KEY=ah_dev \
	HOSPITAL_AGENT_HOSPITAL_URL=http://localhost:4000 \
	HOSPITAL_AGENT_STATE_DIR=$$HOME/.openclaw \
	$(BIN)

clean:
	rm -rf bin

fmt:
	go fmt ./...

vet:
	go vet ./...
