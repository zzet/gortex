BINARY    := gortex

# VERSION defaults to the nearest annotated tag (e.g. v0.1.0) or "dev" when
# no tags exist / not a git checkout. COMMIT is the short SHA; DATE is RFC
# 3339 UTC. internal/version parses main.version and treats main.commit as
# the +build slot, so `gortex version` prints canonical semver.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build build-onnx build-gomlx build-hugot build-windows \
       test bench bench-rpi bench-rpi-quick bench-rpi-profile bench-compare \
       lint fmt clean install dev-link tag-release \
       deps-onnx deps-gomlx deps-hugot deps-vectors \
       claude-plugin claude-plugin-check

# ---------------------------------------------------------------------------
# Build variants
# ---------------------------------------------------------------------------

build:
	go build -ldflags '$(LDFLAGS)' -tags llama -o $(BINARY) ./cmd/gortex/

build-onnx: deps-onnx
	go build -tags embeddings_onnx -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/gortex/

build-gomlx: deps-gomlx
	go build -tags "embeddings_gomlx XLA" -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/gortex/

# Hugot is bundled by default now — this target is kept as a compatibility
# alias and also ensures the dep is explicitly recorded in go.mod.
build-hugot: deps-hugot
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/gortex/

test:
	go test -race ./...

bench:
	go test -bench=. -benchmem -count=1 -benchtime=1s \
		./internal/parser/languages/ \
		./internal/graph/ \
		./internal/search/ \
		./internal/resolver/ \
		./internal/query/ \
		./internal/indexer/ \
		./internal/analysis/

# RPi / low-resource device benchmarks
BENCH_BASELINE ?= results/bench-baseline.txt

bench-rpi:
	./scripts/bench-rpi.sh

bench-rpi-quick:
	./scripts/bench-rpi.sh --quick

bench-rpi-profile:
	./scripts/bench-rpi.sh --profile

bench-compare:
	./scripts/bench-rpi.sh --compare $(BENCH_BASELINE)

bench-save-baseline:
	./scripts/bench-rpi.sh
	@cp $$(ls -t results/bench-*.txt | head -1) $(BENCH_BASELINE)
	@echo "✓ Baseline saved to $(BENCH_BASELINE)"

lint:
	golangci-lint run --timeout=5m

fmt:
	gofmt -s -w .

clean:
	rm -f $(BINARY) gortex.exe gortex-linux gortex-rpi gortex-rpi32

install:
	go install -ldflags '$(LDFLAGS)' ./cmd/gortex/

# dev-link builds the working tree and points the Homebrew shim at it so
# `gortex` on $PATH runs the dev binary. Restarts the daemon so the new
# binary takes over (an old daemon keeps a stale in-memory graph). Revert
# with `brew reinstall gortex`.
HOMEBREW_BIN ?= /opt/homebrew/bin/gortex
dev-link: build
	ln -sfn "$(CURDIR)/$(BINARY)" "$(HOMEBREW_BIN)"

# tag-release stamps the working copy with a git tag that matches the
# version currently in cmd/gortex/main.go. Workflow:
#
#     ./gortex version bump minor   # edits main.go
#     git commit -am "Bump version to v0.2.0"
#     make tag-release              # reads ./gortex, creates tag
#     git push && git push origin v0.2.0
#
# Builds without VERSION ldflags on purpose so `gortex version --short`
# reflects the literal main.go value (not `git describe` drift). Strips
# the +build slot because git tags shouldn't carry build metadata. Emits
# a clear error on dev builds, duplicate tags, or a dirty tree so
# misfires don't silently create broken releases.
tag-release:
	@go build -o $(BINARY) ./cmd/gortex/
	@TAG=$$(./$(BINARY) version --short | sed 's/+.*//'); \
	if [ "$$TAG" = "v0.0.0-dev" ]; then \
		echo "refusing to tag dev build — run \`./$(BINARY) version bump …\` first"; exit 1; \
	fi; \
	if git rev-parse --verify "refs/tags/$$TAG" >/dev/null 2>&1; then \
		echo "tag $$TAG already exists"; exit 1; \
	fi; \
	if ! git diff-index --quiet HEAD --; then \
		echo "tracked files have uncommitted changes — commit the bump first (run \`git status\` to see them)"; exit 1; \
	fi; \
	git tag -a "$$TAG" -m "Release $$TAG"; \
	echo "Tagged $$TAG. Push with: git push origin $$TAG"

# Cross-compile for Raspberry Pi (ARM64)
build-rpi:
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc \
		go build -ldflags '$(LDFLAGS)' -o gortex-rpi ./cmd/gortex/
	@echo "✓ Built gortex-rpi (linux/arm64)"

# Cross-compile for Raspberry Pi (ARMv7 / 32-bit)
build-rpi32:
	CGO_ENABLED=1 GOOS=linux GOARCH=arm GOARM=7 CC=arm-linux-gnueabihf-gcc \
		go build -ldflags '$(LDFLAGS)' -o gortex-rpi32 ./cmd/gortex/
	@echo "✓ Built gortex-rpi32 (linux/arm/v7)"

# Cross-compile for Windows (amd64). Requires the mingw-w64 toolchain
# (`brew install mingw-w64` on macOS, `apt install gcc-mingw-w64` on
# Debian/Ubuntu). CGO stays on because tree-sitter needs a C/C++
# compiler; the llama tag is omitted — the in-process llama.cpp backend
# isn't part of the Windows build. `-extldflags -static` links the
# mingw-w64 C/C++ runtime (libstdc++, libgcc, winpthread) into the .exe
# so it runs on a stock Windows box without bundled DLLs.
build-windows:
	CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
		CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ \
		go build -ldflags '$(LDFLAGS) -extldflags "-static"' -o gortex.exe ./cmd/gortex/
	@echo "✓ Built gortex.exe (windows/amd64)"

# ---------------------------------------------------------------------------
# Marketplace plugin bundle
# ---------------------------------------------------------------------------

# claude-plugin regenerates the Anthropic Plugin Marketplace bundle at
# claude-plugin/. The bundle is checked in so the marketplace's
# "git-subdir" source can pull it directly. The CI guard
# (claude-plugin-check) asserts that re-running this target produces
# no diff against what's checked in — drift means the bundle is stale
# vs the source-of-truth content in
# internal/agents/claudecode/content.go.
claude-plugin: build
	./$(BINARY) plugin emit --target ./claude-plugin --variant anthropic
	@echo "✓ Regenerated claude-plugin/ from internal/agents/claudecode content"

claude-plugin-check: claude-plugin
	@if ! git diff --exit-code -- claude-plugin >/dev/null 2>&1; then \
		echo "claude-plugin/ is out of date — run 'make claude-plugin' and commit the result"; \
		git --no-pager diff --stat -- claude-plugin; \
		exit 1; \
	fi
	@echo "✓ claude-plugin/ matches generated output"

# ---------------------------------------------------------------------------
# Embedding backend dependencies
# ---------------------------------------------------------------------------

# ONNX Runtime — system library required for -tags embeddings_onnx
deps-onnx:
	@echo "=== ONNX Runtime dependency ==="
ifeq ($(shell uname -s),Darwin)
	@command -v brew >/dev/null 2>&1 || { echo "Error: Homebrew required. Install from https://brew.sh"; exit 1; }
	@brew list onnxruntime >/dev/null 2>&1 || brew install onnxruntime
	@echo "✓ onnxruntime installed (macOS/Homebrew)"
else ifeq ($(shell uname -s),Linux)
	@dpkg -s libonnxruntime-dev >/dev/null 2>&1 || { echo "Run: sudo apt install libonnxruntime-dev"; exit 1; }
	@echo "✓ libonnxruntime-dev installed (Linux/apt)"
endif
	go get github.com/yalue/onnxruntime_go@latest
	@echo "✓ Go ONNX bindings ready"

# GoMLX — XLA/PJRT plugin auto-downloads on first run (~100MB)
deps-gomlx:
	@echo "=== GoMLX dependency ==="
	go get github.com/gomlx/gomlx@latest
	go get github.com/gomlx/onnx-gomlx@latest
	@echo "=== rust tokenizers (the XLA session links libtokenizers.a statically) ==="
	@if [ -f /usr/lib/libtokenizers.a ] || [ -f /usr/local/lib/libtokenizers.a ]; then \
		echo "✓ libtokenizers.a already present"; \
	else \
		os=$$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$$(uname -m); \
		case "$$os-$$arch" in \
			darwin-arm64) asset=libtokenizers.darwin-arm64.tar.gz; dest=/usr/local/lib ;; \
			darwin-x86_64) asset=libtokenizers.darwin-x86_64.tar.gz; dest=/usr/local/lib ;; \
			linux-x86_64|linux-amd64) asset=libtokenizers.linux-amd64.tar.gz; dest=/usr/lib ;; \
			linux-aarch64|linux-arm64) asset=libtokenizers.linux-arm64.tar.gz; dest=/usr/lib ;; \
			*) echo "No prebuilt libtokenizers for $$os-$$arch — build it from https://github.com/daulet/tokenizers and place libtokenizers.a on the linker path"; exit 1 ;; \
		esac; \
		echo "  downloading $$asset -> $$dest"; \
		curl -fsSL "https://github.com/daulet/tokenizers/releases/download/v1.27.0/$$asset" | tar -xz -C /tmp; \
		sudo cp /tmp/libtokenizers.a "$$dest/"; \
	fi
	@echo "✓ GoMLX + ONNX converter + rust tokenizers installed"
	@echo "  Note: XLA/PJRT plugin will auto-download on first run (~100MB)"

# Hugot — uses same XLA/PJRT backend as GoMLX
deps-hugot:
	@echo "=== Hugot dependency ==="
	go get github.com/knights-analytics/hugot@latest
	@echo "✓ Hugot installed"
	@echo "  Note: XLA/PJRT plugin will auto-download on first run (~100MB)"

# Prepare GloVe word vectors for built-in static embeddings
deps-vectors:
	@echo "=== Preparing GloVe word vectors ==="
	@test -f internal/embedding/data/vectors.bin.gz && echo "✓ Vectors already prepared" || bash scripts/prepare_vectors.sh

# ---------------------------------------------------------------------------
# Eval framework
# ---------------------------------------------------------------------------

EVAL_DIR     := eval
EVAL_VENV    := $(EVAL_DIR)/.venv
EVAL_PYTHON  := $(EVAL_VENV)/bin/python
EVAL_PIP     := $(EVAL_VENV)/bin/pip
EVAL_CLI     := $(EVAL_VENV)/bin/gortex-eval
EVAL_ANALYZE := $(EVAL_VENV)/bin/gortex-eval-analyze

MODEL  ?= claude-sonnet
MODE   ?= baseline
SLICE  ?= 0:5
SUBSET ?= lite

.PHONY: eval-setup eval-test eval-test-all eval-list \
        eval-single eval-matrix eval-debug eval-summary eval-compare eval-tools

# Setup: create venv and install deps
eval-setup: build
	@test -d $(EVAL_VENV) || python3 -m venv $(EVAL_VENV)
	$(EVAL_PIP) install -q -e "$(EVAL_DIR)[dev]"
	@echo "✓ Eval framework ready. Binary: ./$(BINARY)"

# Build linux/amd64 binary for container injection (requires podman/docker)
eval-build-linux:
	podman run --rm --platform linux/amd64 -v $(CURDIR):/src -w /src golang:1.27 \
		bash -c "apt-get update -qq && apt-get install -y -qq libtree-sitter-dev && go build -ldflags '$(LDFLAGS)' -o gortex-linux ./cmd/gortex/"
	@echo "✓ Built gortex-linux (linux/amd64)"

# Run Python eval tests
eval-test:
	$(EVAL_PYTHON) -m pytest $(EVAL_DIR)/tests/ -q

# Run all tests (Go + Python)
eval-test-all: test eval-test

# List available configs
eval-list: eval-setup
	$(EVAL_CLI) list-configs

# Single (model, mode) run
eval-single: eval-setup
	$(EVAL_CLI) single -m $(MODEL) --mode $(MODE) --subset $(SUBSET) --slice $(SLICE)

# Full A/B matrix
eval-matrix: eval-setup
	$(EVAL_CLI) matrix --models claude-sonnet claude-haiku \
		--modes baseline native native_augment \
		--subset $(SUBSET) --slice $(SLICE)

# Debug a single instance
eval-debug: eval-setup
	$(EVAL_CLI) debug -m $(MODEL) --mode $(MODE) -i $(INSTANCE)

# Analyze results
eval-summary:
	$(EVAL_ANALYZE) summary results/

eval-compare:
	$(EVAL_ANALYZE) compare-modes results/ -m $(MODEL)

eval-tools:
	$(EVAL_ANALYZE) tool-usage results/
