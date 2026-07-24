# Technology Stack

**Analysis Date:** 2026-07-24

## Languages

**Primary:**
- Go 1.26.5 - Core engine for code-intelligence daemon, CLI, HTTP server, and MCP protocol implementation

**Secondary:**
- TypeScript/React - Web UI (Next.js 15) for graph visualization and dashboards
- Bash/Shell - Build scripts, installation, and benchmarking

## Runtime

**Environment:**
- Go 1.26.5 runtime
- Single statically-linked binary for macOS (Intel/ARM64), Linux (x86_64/ARM64), Windows

**Package Manager:**
- Go modules - `go.mod` with 70+ direct dependencies
- Lockfile: `go.sum` (81KB+) - Present and committed

## Frameworks

**Core:**
- Cobra 1.10.2 - CLI command framework and subcommand routing
- Viper 1.21.0 - Configuration management, environment variables, file parsing
- Pflag 1.0.10 - Command-line flag parsing with POSIX compatibility

**Parsing & Code Analysis:**
- Tree-sitter 0.25.0+ - 257 language parsers via tree-sitter-forest (alexaandru/go-sitter-forest)
  - Core grammars: Go, Python, TypeScript, Rust, Java, C, C++, C#, PHP, Ruby, JavaScript
  - Language variants: Ada, COBOL, Haskell, Kotlin, OCaml, Scala, Swift, Elixir, and 240+ more
  - Gortex-maintained fork: tree-sitter-SQL, tree-sitter-Protobuf, tree-sitter-Dockerfile, tree-sitter-Markdown, tree-sitter-Dart, tree-sitter-OrgMode
- AST Query - In-process AST pattern matching and semantic analysis (`internal/astquery/`)

**Data & Search:**
- Bleve v2 6.0.0 - Full-text search with BM25 algorithm
  - BM25+ ranking for semantic relevance
  - Distributed indexing via scorch_segment_api
  - HNSW (Hierarchical Navigable Small World) integration via coder/hnsw 0.6.1
- SQLite 1.54.0 (modernc.org) - Persistent graph snapshots, session data, conversation logs
- bbolt 1.5.0 - Embedded key-value store (BoltDB)
- PostgreSQL pgx/v5 5.10.0 - Optional remote graph storage backend

**Embedding & Vectors:**
- Hugot 0.7.5 - Pure-Go ONNX runtime for transformer models; auto-downloads MiniLM-L6-v2
- ONNX Runtime (yalue/onnxruntime_go 1.31.0) - Optional native backend (embeddings_onnx build tag)
- GoMLX + XLA (gomlx/go-xla 0.2.2, gomlx/gomlx 0.27.3) - Optional GPU-accelerated inference (embeddings_gomlx build tag)
- GloVe 50-dimensional word vectors - Default static embedding (3.8MB embedded)
- Tokenizers (pkoukk/tiktoken-go 0.1.8, pkoukk/tiktoken-go-loader 0.0.2) - Token counting and OpenAI-compatible tokenization

**Terminal UI:**
- Charmbracelet Bubbletea 1.3.10 - TUI framework (interactive dashboards, progress)
- Charmbracelet Bubbles 1.0.0 - Reusable TUI components (tables, inputs, lists)
- Charmbracelet Lipgloss 1.1.0 - Terminal styling (colors, borders, alignment)
- Charmbracelet x/ansi 0.11.7 - ANSI escape sequence utilities

**HTTP & Web:**
- Go net/http - Standard library HTTP server
- CORS handling - Custom implementation (`internal/server/cors.go`)
- Streamable HTTP - MCP 2026 protocol support via mark3labs/mcp-go 0.56.0

**MCP Protocol:**
- mark3labs/mcp-go 0.56.0 - Model Context Protocol server implementation
- STDIO and HTTP transport modes

**Testing:**
- Testify v1.11.1 (stretchr) - Assertion and mocking library
- rapid 1.3.0 (pgregory.net) - Property-based testing framework
- Go's built-in `testing` package

**Build & Distribution:**
- Goreleaser - Cross-platform binary building and release packaging
- golangci-lint 2.11.4 - Multi-linter runner
- GCX1 wire format (gortexhq/gcx-go 0.1.0) - Compact 27% space-efficient serialization

**Logging & Monitoring:**
- go.uber.org/zap 1.28.0 - Structured logging with performance optimization
- Telemetry - Optional anonymous tool/command counts (off by default)

## Key Dependencies

**Critical:**
- `github.com/tree-sitter/go-tree-sitter v0.25.0` - Tree-sitter C bindings. **Note:** Uses CGO; vendored `go-pointer` shim replaces upstream to unlock multi-goroutine parsing (see `internal/thirdparty/go-pointer`)
- `github.com/blevesearch/bleve/v2 v2.6.0` - Full-text indexing and semantic search engine
- `github.com/coder/hnsw v0.6.1` - Vector similarity search
- `github.com/knights-analytics/hugot v0.7.5` - Model inference and embedding generation

**Persistence:**
- `modernc.org/sqlite v1.54.0` - Pure-Go SQLite implementation (no external C dependency)
- `github.com/jackc/pgx/v5 v5.10.0` - PostgreSQL driver (optional remote backend)
- `go.etcd.io/bbolt v1.5.0` - Key-value store

**External APIs & Integrations:**
- `github.com/google/go-github/v88 v88.0.0` - GitHub API client (PR review, contract detection)
- `github.com/gomlx/go-huggingface v0.3.5` - Hugging Face model API integration
- `github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728` - PDF parsing and extraction

**Embeddings & Tokenization:**
- `github.com/pkoukk/tiktoken-go v0.1.8` - OpenAI tokenizer
- `github.com/pkoukk/tiktoken-go-loader v0.0.2` - Tokenizer vocabulary loader

**CLI & Configuration:**
- `github.com/spf13/cobra v1.10.2` - Command-line interface framework
- `github.com/spf13/viper v1.21.0` - Configuration file parsing
- `github.com/spf13/pflag v1.0.10` - Flag parsing

**File System & Git:**
- `github.com/fsnotify/fsnotify v1.10.1` - File system watcher (for live indexing)
- `github.com/sabhiram/go-gitignore v0.0.0-20210923224102-525f6e181f06` - .gitignore parsing
- `github.com/gofrs/flock v0.13.0` - File locking (daemon lock)

**Data Structures & Algorithms:**
- `github.com/RoaringBitmap/roaring/v2 v2.19.0` - Efficient integer set operations
- `github.com/bits-and-blooms/bitset v1.24.6` - Bitset implementation
- `github.com/zeebo/blake3 v0.2.4` - BLAKE3 hashing

**Utilities:**
- `github.com/google/uuid v1.6.0` - UUID generation
- `github.com/pelletier/go-toml/v2 v2.4.3` - TOML parsing
- `gopkg.in/yaml.v3 v3.0.1` - YAML parsing
- `github.com/jedib0t/go-pretty/v6 v6.8.3` - Pretty table formatting
- `github.com/toon-format/toon-go v0.0.0-20251202084852-7ca0e27c4e8c` - TOON wire format support

## Configuration

**Environment:**
- `.gortex.yaml` - Project-level indexing and query configuration (languages, exclusions, workers, query depth)
  - Example: `internal/llm/config.go` (LLM provider configuration)
  - Supports multiple LLM provider backends (anthropic, openai, ollama, local, azure, gemini, bedrock, deepseek)
- Viper-based config merging (YAML files, env vars, flags)
- `.env` file support via viper (environment-based secrets, API keys)
- CLI flags override config file values

**Build:**
- Makefile with build variants:
  - `make build` - Default with llama tag for local LLM support
  - `make build-onnx` - With native ONNX runtime (requires libonnxruntime on PATH)
  - `make build-gomlx` - With GPU-accelerated XLA/PJRT backend
- Goreleaser config (`.goreleaser.yml`) - Linux/macOS/Windows cross-compilation with CGO
- LDFLAGS injection: version, commit SHA, build date

## Platform Requirements

**Development:**
- Go 1.26.5+
- CGO enabled for tree-sitter parsing (requires C/C++ compiler)
- On Linux: cross-compile toolchain via goreleaser-cross Docker image
- On macOS: Apple native ld for binary linking (OSX 15+/Tahoe dyld compatibility)
- On Windows: mingw C/C++ toolchain for CGO

**Production:**
- **Deployment target:** Single statically-linked binary (no runtime dependencies)
- **Databases:** SQLite bundled; PostgreSQL optional for multi-instance deployments
- **Disk:** Graph snapshots stored in project-root `.gortex/` directory (configurable via workspace config)
- **Memory:** Scales with repository size; precomputed reach index keeps depth-3 queries O(N) in fan-in/out
- **OS Support:** Linux (glibc 2.29+), macOS 12+, Windows 10+
- **Network:** Optional (daemon-only mode); HTTP server if MCP/web UI enabled

---

*Stack analysis: 2026-07-24*
