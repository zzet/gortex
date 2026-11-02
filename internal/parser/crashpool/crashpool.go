// Package crashpool runs tree-sitter extraction inside isolated worker
// subprocesses so a grammar SIGSEGV / OOM / hang on one pathological
// file cannot abort the whole index pass.
//
// A worker that dies mid-parse is detected by the parent as a broken
// pipe; the in-flight file is quarantined and a fresh worker is spawned
// to drain the rest of the queue. A worker that merely hangs is killed
// on a per-request deadline and treated the same way. A recovered Go
// panic in an extractor comes back as an ordinary error response and
// the worker stays alive.
//
// Quarantined files are persisted (Quarantine) so a file that crashes
// the parser survives daemon restarts and is skipped until its content
// changes — at which point it gets exactly one retry.
package crashpool

import (
	"encoding/gob"

	"github.com/zzet/gortex/internal/graph"
)

func init() {
	// Node.Meta / Edge.Meta carry these concrete types inside their
	// interface values; gob needs every concrete type registered to
	// encode an interface. Mirrors the snapshot codec registrations in
	// internal/persistence.
	gob.Register(map[string]any{})
	gob.Register([]any{})
	gob.Register([]string{})
	gob.Register([]int{})
	gob.Register([]map[string]string{})
	gob.Register([]map[string]any{})
}

// extractRequest is one unit of parse work sent parent → worker.
type extractRequest struct {
	Seq                uint64
	RelPath            string
	Language           string
	Content            []byte
	TemporalEnvHelpers []string
}

// extractResponse is the worker → parent reply for one request.
type extractResponse struct {
	Seq          uint64
	Nodes        []*graph.Node
	Edges        []*graph.Edge
	ParseErrors  int
	ParseChecked bool
	HasParseErr  bool
	// Err is non-empty when the extractor returned an error or
	// panicked. Panicked distinguishes a recovered Go panic (the
	// worker survives) from a plain error return.
	Err      string
	Panicked bool
}

// Result is the parent-side outcome of one Submit call.
type Result struct {
	Nodes        []*graph.Node
	Edges        []*graph.Edge
	ParseErrors  int
	ParseChecked bool
	HasParseErr  bool
	// Crashed is true when the worker subprocess died or hung
	// (SIGSEGV / OOM / kill / deadline). The file should be
	// quarantined and skipped.
	Crashed bool
	// Panicked is true when an extractor panicked but was recovered:
	// the file is bad but the worker survived.
	Panicked bool
	// Err carries the failure detail for Crashed / Panicked / error
	// responses.
	Err string
}

// Bad reports whether the file failed to parse (crash, hang, panic, or
// extractor error) and should be quarantined.
func (r Result) Bad() bool { return r.Crashed || r.Panicked || r.Err != "" }
