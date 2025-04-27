GO := env GO111MODULE=on go
GOBUILD := $(GO) build
GOTEST := $(GO) test
BIN_DIR := build

TARGET := celestia
TARGETS := $(TARGET)

all: build test
.PHONY: all

build: $(TARGETS)
.PHONY: build

$(TARGETS):
	mkdir -p $(BIN_DIR)
	$(GOBUILD) -v -o $(BIN_DIR)/ ./cmd/$@

test: $(TARGETS)
	$(GOTEST) -mod=readonly ./...
.PHONY: test
