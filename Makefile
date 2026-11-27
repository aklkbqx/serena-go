BIN ?= $(HOME)/.local/bin/serena-go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo local)
LDFLAGS := -s -w -X main.buildVersion=$(VERSION) -X main.buildCommit=$(COMMIT)

.PHONY: test vet check build install build-all clean

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

check:
	./scripts/check-oss-hygiene.sh

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o serena-go .

install: build
	mkdir -p $(dir $(BIN))
	mv serena-go $(BIN).new
	mv $(BIN).new $(BIN)
	@ls -lh $(BIN)

build-all:
	./scripts/release.sh --version $(VERSION)

clean:
	rm -rf dist serena-go serena-go.new
