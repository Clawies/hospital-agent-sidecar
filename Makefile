BIN := bin/hospital-agent
LINUX_BIN := bin/hospital-agent-linux-amd64
PKG := ./cmd/hospital-agent

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-linux run clean fmt vet

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

build-linux:
	@mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(LINUX_BIN) $(PKG)

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
