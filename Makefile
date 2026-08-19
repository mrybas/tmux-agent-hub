.PHONY: build test vet install-hooks uninstall-hooks clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/tmux-agent-hub ./cmd/tmux-agent-hub

test:
	go test ./...

vet:
	go vet ./...

# Registering hooks must not need a toolchain: the plugin downloads a
# release binary when Go is absent, and that binary can do this itself.
install-hooks:
	@[ -x bin/tmux-agent-hub ] || $(MAKE) build
	./bin/tmux-agent-hub install-hooks

uninstall-hooks:
	@[ -x bin/tmux-agent-hub ] || $(MAKE) build
	./bin/tmux-agent-hub uninstall-hooks

clean:
	rm -rf bin
