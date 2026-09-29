GO_BIN ?= $(HOME)/.local/share/mise/installs/go/1.25.7/bin/go
WAILS3_BIN ?= $(HOME)/.local/share/mise/installs/go/1.25.7/bin/wails3
GO_TOOLS_DIR := $(dir $(GO_BIN))
DESKTOP_DEV_DATA_DIR ?= $(HOME)/.lm-router-dev
DESKTOP_DEV_PORT ?= 19091

.PHONY: cli test desktop-dev desktop-mac desktop-linux

cli:
	CGO_ENABLED=0 $(GO_BIN) build -o lm-router ./cmd/lm-router

test:
	$(GO_BIN) test ./cmd/... ./internal/...

desktop-dev:
	LM_ROUTER_PROFILE=development LM_ROUTER_DATA_DIR="$(DESKTOP_DEV_DATA_DIR)" LM_ROUTER_PORT="$(DESKTOP_DEV_PORT)" PATH="$(GO_TOOLS_DIR):$$PATH" $(WAILS3_BIN) dev -config ./desktop/build/config.yml

desktop-mac:
	PATH="$(GO_TOOLS_DIR):$$PATH" $(WAILS3_BIN) task package:mac:universal

desktop-linux:
	PATH="$(GO_TOOLS_DIR):$$PATH" $(WAILS3_BIN) task build:linux
