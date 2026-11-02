package crashpool

import (
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// RunWorker is the subprocess entry point. It serves extract requests
// decoded from r and writes gob-encoded responses to w until r reaches
// EOF, then returns nil.
//
// It is invoked by the hidden `gortex __parse-worker` subcommand. The
// parent (Pool) owns the lifecycle: a worker is expected to run until
// its stdin is closed or it is killed.
func RunWorker(r io.Reader, w io.Writer) error {
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	registerWorkerGrammars(reg)
	registerWorkerExtractorPlugins(reg)
	registerWorkerFallbackChunkers(reg)
	return serveWorker(reg, r, w)
}

// registerWorkerGrammars loads the custom tree-sitter grammars the
// parent passed through the GORTEX_CUSTOM_GRAMMARS environment
// variable — a JSON array of config.GrammarSpec — so a crash-isolated
// worker resolves the same languages as the parent process. A malformed
// or absent value is silently ignored: the worker simply has no custom
// grammars, exactly as before this channel existed.
func registerWorkerGrammars(reg *parser.Registry) {
	raw := os.Getenv("GORTEX_CUSTOM_GRAMMARS")
	if raw == "" {
		return
	}
	var specs []config.GrammarSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return
	}
	languages.RegisterCustomGrammars(reg, specs, nil)
}

// registerWorkerExtractorPlugins loads the subprocess extractor plugins
// the parent passed through the GORTEX_EXTRACTOR_PLUGINS environment
// variable — a JSON array of config.ExtractorPluginSpec — so a
// crash-isolated worker resolves the same plugin languages as the
// parent. A malformed or absent value is silently ignored.
func registerWorkerExtractorPlugins(reg *parser.Registry) {
	raw := os.Getenv("GORTEX_EXTRACTOR_PLUGINS")
	if raw == "" {
		return
	}
	var specs []config.ExtractorPluginSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return
	}
	languages.RegisterExtractorPlugins(reg, specs, nil)
}

// registerWorkerFallbackChunkers loads the regex fallback chunkers the
// parent passed through the GORTEX_FALLBACK_CHUNKERS environment
// variable — a JSON array of config.FallbackChunkerSpec — so a
// crash-isolated worker resolves the same grammar-less languages as the
// parent. A malformed or absent value is silently ignored.
func registerWorkerFallbackChunkers(reg *parser.Registry) {
	raw := os.Getenv("GORTEX_FALLBACK_CHUNKERS")
	if raw == "" {
		return
	}
	var specs []config.FallbackChunkerSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		return
	}
	languages.RegisterFallbackChunkers(reg, specs, nil)
}

// serveWorker is the decode/extract/encode loop, factored out of
// RunWorker so tests can drive it with an in-memory registry.
func serveWorker(reg *parser.Registry, r io.Reader, w io.Writer) error {
	dec := gob.NewDecoder(r)
	enc := gob.NewEncoder(w)
	for {
		var req extractRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		resp := serveOne(reg, req)
		if err := enc.Encode(&resp); err != nil {
			return err
		}
	}
}

// serveOne runs one extraction under panic recovery. A Go panic in an
// extractor (a bug, deep recursion, an out-of-range index) is converted
// to an error response so the parent can quarantine the file without
// losing the worker. A hard fault (SIGSEGV in the C grammar, OOM kill)
// is not recoverable here — it kills the process, and the parent
// detects it as a pipe EOF.
func serveOne(reg *parser.Registry, req extractRequest) (resp extractResponse) {
	resp.Seq = req.Seq
	defer func() {
		if rec := recover(); rec != nil {
			resp.Nodes = nil
			resp.Edges = nil
			resp.Err = fmt.Sprintf("extractor panic: %v\n%s", rec, debug.Stack())
			resp.Panicked = true
			resp.ParseChecked, resp.ParseErrors = false, 0
		}
	}()

	ext, ok := reg.GetByLanguage(req.Language)
	if !ok || ext == nil {
		resp.Err = "crashpool: no extractor for language " + req.Language
		return resp
	}
	opts := parser.NewExtractionOptions(req.TemporalEnvHelpers)
	result, err := parser.Extract(ext, req.RelPath, req.Content, opts)
	if err != nil {
		resp.Err = err.Error()
		return resp
	}
	if result != nil {
		resp.Nodes = result.Nodes
		resp.Edges = result.Edges
		if result.ParseChecked && result.ParseErrors >= 0 {
			resp.ParseChecked, resp.ParseErrors = true, result.ParseErrors
			resp.HasParseErr = result.ParseErrors > 0
		} else if result.Tree != nil && result.Tree.HasParseErrors() {
			// Legacy/plugin trees retain their old error metadata, but absence
			// of an explicit full-parse proof cannot become checked zero.
			resp.HasParseErr, resp.ParseErrors = true, result.Tree.CountParseErrors()
		}
		result.ReleaseTree()
	}
	return resp
}
