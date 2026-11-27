BIN ?= $(HOME)/.local/bin/serena-go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.2.0-dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo local)
LDFLAGS := -s -w -X main.buildVersion=$(VERSION) -X main.buildCommit=$(COMMIT)
# Curated gotreesitter language subset (~8-12 MB binary vs 33 MB full 206-language build).
# Pass BUILD_TAGS= to include all 206 languages.
BUILD_TAGS ?= grammar_subset,grammar_subset_go,grammar_subset_typescript,grammar_subset_tsx,grammar_subset_javascript,grammar_subset_python,grammar_subset_rust,grammar_subset_swift,grammar_subset_c,grammar_subset_cpp,grammar_subset_objc,grammar_subset_java,grammar_subset_kotlin,grammar_subset_sql,grammar_subset_bash,grammar_subset_ruby,grammar_subset_php,grammar_subset_vue,grammar_subset_svelte,grammar_subset_json,grammar_subset_yaml,grammar_subset_toml,grammar_subset_markdown,grammar_subset_css,grammar_subset_html,grammar_subset_dockerfile

.PHONY: test vet check build install build-all clean

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

check:
	./scripts/check-oss-hygiene.sh

build:
	CGO_ENABLED=0 go build -trimpath $(if $(BUILD_TAGS),-tags "$(BUILD_TAGS)",) -ldflags="$(LDFLAGS)" -o serena-go .

install: build
	@target="$(BIN)"; \
	if [ -L "$$target" ]; then target="$$(readlink "$$target")"; case "$$target" in /*) ;; *) target="$$(dirname "$(BIN)")/$$target" ;; esac; fi; \
	mkdir -p "$$(dirname "$$target")"; \
	mv serena-go "$$target.new" && mv "$$target.new" "$$target"; \
	ls -lh "$$target"

build-all:
	./scripts/release.sh --version $(VERSION)

clean:
	rm -rf dist serena-go serena-go.new
