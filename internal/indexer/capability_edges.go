package indexer

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// processExecAPIs maps a callee — the dotted path as it appears in an
// unresolved call target — to the canonical process-execution mechanism
// it represents. Covers the os/exec, subprocess, child_process, and
// process-builder families across Go, Python, JS/TS, Rust, and Java.
var processExecAPIs = map[string]string{
	// Go
	"exec.Command": "exec.Command", "exec.CommandContext": "exec.CommandContext",
	"os.StartProcess": "os.StartProcess", "syscall.Exec": "syscall.Exec",
	"syscall.ForkExec": "syscall.ForkExec",
	// Python
	"subprocess.run": "subprocess.run", "subprocess.Popen": "subprocess.Popen",
	"subprocess.call": "subprocess.call", "subprocess.check_call": "subprocess.check_call",
	"subprocess.check_output": "subprocess.check_output", "subprocess.getoutput": "subprocess.getoutput",
	"os.system": "os.system", "os.popen": "os.popen",
	// JS / TS
	"child_process.exec": "child_process.exec", "child_process.execSync": "child_process.execSync",
	"child_process.spawn": "child_process.spawn", "child_process.spawnSync": "child_process.spawnSync",
	"child_process.execFile": "child_process.execFile", "child_process.execFileSync": "child_process.execFileSync",
	"child_process.fork": "child_process.fork",
	// Rust
	"Command::new": "Command::new", "process::Command::new": "Command::new",
	// Java
	"Runtime.exec": "Runtime.exec",
}

// processExecBareNames are unqualified callee names that strongly imply
// process execution regardless of receiver — PHP shell builtins and the
// destructured-import JS forms (const {execSync} = require('child_process')).
// Generic words (system, exec, spawn, popen) are intentionally excluded to
// avoid false positives on unrelated same-named calls.
var processExecBareNames = map[string]string{
	"shell_exec": "shell_exec", "passthru": "passthru", "proc_open": "proc_open",
	"execSync": "child_process.execSync", "spawnSync": "child_process.spawnSync",
	"execFileSync": "child_process.execFileSync",
}

// knownExecSchemes are the resolver's external-symbol ID scheme tokens.
// A resolved call target carries one (optionally behind a <repo>:: prefix)
// as in stdlib::os/exec::Command — see external_call_attribution.go.
var knownExecSchemes = map[string]bool{
	"stdlib": true, "dep": true, "module": true, "external": true,
}

// execCalleeCandidates yields the spellings of a call target to test
// against the exec tables. The resolver may leave a call unresolved
// (unresolved::exec.Command), resolve it onto a fully-qualified external
// node ID (stdlib::os/exec::Command, optionally <repo>::-prefixed), or
// keep a Rust-style path spelling (Command::new). Returning both the
// as-written form and the collapsed pkg.Symbol form lets one matcher
// cover all of them — so Go os/exec calls (the common case) produce an
// executes_process edge whether or not the import resolved.
func execCalleeCandidates(to string) []string {
	to = strings.TrimPrefix(to, "unresolved::")
	cands := []string{to}
	segs := strings.Split(to, "::")
	for i, s := range segs {
		if !knownExecSchemes[s] {
			continue
		}
		rest := segs[i+1:]
		switch {
		case len(rest) >= 2:
			// <scheme>::<import/path>::<Symbol> -> pkg.Symbol, keeping the
			// last path segment as the import alias the source wrote
			// (os/exec -> exec). Rust's Command::new has no scheme token,
			// so it never reaches here and keeps its as-written spelling.
			sym := rest[len(rest)-1]
			pkg := rest[len(rest)-2]
			if k := strings.LastIndex(pkg, "/"); k >= 0 {
				pkg = pkg[k+1:]
			}
			if pkg != "" && sym != "" {
				cands = append(cands, pkg+"."+sym)
			}
		case len(rest) == 1 && rest[0] != "":
			cands = append(cands, rest[0])
		}
		break
	}
	return cands
}

// processExecMechanism returns the canonical process-execution mechanism
// for a callee, or "" when the callee is not a recognised exec API. It
// accepts either an as-written callee or a resolved external node ID.
func processExecMechanism(callee string) string {
	for _, c := range execCalleeCandidates(callee) {
		if m := processExecAPIs[c]; m != "" {
			return m
		}
		last := c
		if i := strings.LastIndexAny(c, "."); i >= 0 {
			last = c[i+1:]
		}
		if m := processExecBareNames[last]; m != "" {
			return m
		}
	}
	return ""
}

// synthesizeCapabilityEdges materialises the three first-class capability
// edge kinds (NEW-KNW-3) from edges the language extractors already emit,
// so a supply-chain / least-privilege audit can traverse one edge kind
// instead of joining through the config, dataflow, and call layers:
//
//   - EdgeReadsEnv: every reads_config edge whose target is a cfg::env::
//     node, re-pointed at the same typed env-var node.
//   - EdgeAccessesField: every reads / writes edge that lands on a
//     KindField node, with Meta["access"] = read|write.
//   - EdgeExecutesProcess: every calls edge whose callee is a known
//     process-exec API, pointed at a synthetic typed process node
//     (string::process::<mechanism>).
//
// It runs after the resolver (so call/field targets are settled) and is
// idempotent — AddEdge dedupes by edge key and a reindex re-derives from
// the current base edges. Returns per-kind counts for telemetry.
func synthesizeCapabilityEdges(g graph.Store) (readsEnv, execProc, fieldAccess int) {
	return synthesizeCapabilityEdgesScoped(g, nil)
}

// synthesizeCapabilityEdgesScoped is synthesizeCapabilityEdges restricted to the
// changed repos in an end-of-batch pass. A nil scope runs the whole-graph pass,
// so the fresh-index / single-repo path is byte-identical.
//
// Correctness: every capability edge is FROM the code symbol that performs the
// read/write/call, so a changed repo owns exactly the capability edges its
// reindex dropped; an unchanged repo's are already on disk and are not
// re-derived. The driving scan therefore walks only the changed repos'
// out-edges (GetRepoEdges — one backend query per repo) instead of four
// whole-graph EdgesByKind sweeps. Edge TARGETS may live in any repo (a field or
// env node an unchanged sibling defines), so target identity is never scoped: a
// field target outside the changed repos is confirmed by a cached per-id lookup
// rather than a whole-graph KindField map. Indirect field mutations stay
// whole-graph — their transitive fixpoint lives in indirectMutationEdges (a
// file this pass does not own) and re-affirming an unchanged repo's indirect
// edges is idempotent via the add() dedup.
func synthesizeCapabilityEdgesScoped(g graph.Store, changedPrefixes map[string]bool, changedFiles ...string) (readsEnv, execProc, fieldAccess int) {
	if g == nil {
		return 0, 0, 0
	}
	g.ResolveMutex().Lock()
	defer g.ResolveMutex().Unlock()
	if len(changedFiles) > 0 {
		return synthesizeCapabilityEdgesForFiles(g, changedFiles)
	}

	type edgeKey struct {
		from, to, via string
		kind          graph.EdgeKind
	}
	type edgeSpec struct {
		from, to, origin, file string
		line                   int
		kind                   graph.EdgeKind
		meta                   map[string]any
	}
	type sourceOrder struct {
		repo     string
		identity graph.EdgeIdentity
	}
	type baseSpec struct {
		edgeSpec
		source        sourceOrder
		access        string
		procMechanism string
	}

	var pending []edgeSpec
	seen := map[edgeKey]bool{}
	add := func(from, to string, kind graph.EdgeKind, origin, file string, line int, meta map[string]any) bool {
		key := edgeKey{from: from, to: to, kind: kind}
		if via, _ := meta["via"].(string); via != "" {
			key.via = via
		}
		if seen[key] {
			return false
		}
		seen[key] = true
		pending = append(pending, edgeSpec{from: from, to: to, origin: origin, file: file, line: line, kind: kind, meta: meta})
		return true
	}
	lessSource := func(left, right sourceOrder) bool {
		if left.repo != right.repo {
			return left.repo < right.repo
		}
		a, b := left.identity, right.identity
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		return a.Line < b.Line
	}

	// nil means an unscoped whole-graph scan; a non-nil empty slice means an
	// exact empty scope and must remain a no-op.
	var repoPrefixes []string
	if changedPrefixes != nil {
		repoPrefixes = make([]string, 0, len(changedPrefixes))
	}
	for prefix := range changedPrefixes {
		repoPrefixes = append(repoPrefixes, prefix)
	}
	sort.Strings(repoPrefixes)

	// The production SQLite scanner keyset-pages a seven-column projection and
	// filters non-field reads/writes in SQL. It deliberately does not decode
	// confidence, provenance, or Meta. The fallback preserves adapter behavior.
	baseByKey := make(map[edgeKey]baseSpec)
	graph.ScanRepoCapabilityEdges(g, repoPrefixes, 0, func(page []graph.RepoCapabilityEdge) bool {
		for _, row := range page {
			input := row.Identity
			candidate := baseSpec{
				edgeSpec: edgeSpec{
					from: input.From, to: input.To,
					file: input.FilePath, line: input.Line,
				},
				source: sourceOrder{repo: row.RepoPrefix, identity: input},
			}
			switch input.Kind {
			case graph.EdgeReadsConfig:
				if !strings.Contains(input.To, "cfg::env::") {
					continue
				}
				candidate.kind = graph.EdgeReadsEnv
				candidate.origin = graph.OriginASTResolved
			case graph.EdgeReads:
				candidate.kind = graph.EdgeAccessesField
				candidate.origin = graph.OriginASTResolved
				candidate.access = "read"
			case graph.EdgeWrites:
				candidate.kind = graph.EdgeAccessesField
				candidate.origin = graph.OriginASTResolved
				candidate.access = "write"
			case graph.EdgeCalls:
				mechanism := processExecMechanism(input.To)
				if mechanism == "" {
					continue
				}
				candidate.to = "string::process::" + mechanism
				candidate.kind = graph.EdgeExecutesProcess
				candidate.origin = graph.OriginASTInferred
				candidate.procMechanism = mechanism
			default:
				continue
			}

			key := edgeKey{from: candidate.from, to: candidate.to, kind: candidate.kind}
			current, exists := baseByKey[key]
			if !exists || lessSource(candidate.source, current.source) {
				baseByKey[key] = candidate
			}
		}
		return true
	})

	base := make([]baseSpec, 0, len(baseByKey))
	for _, candidate := range baseByKey {
		base = append(base, candidate)
	}
	sort.Slice(base, func(i, j int) bool {
		return lessSource(base[i].source, base[j].source)
	})

	procNodes := map[string]*graph.Node{}
	for _, candidate := range base {
		var meta map[string]any
		if candidate.access != "" {
			meta = map[string]any{"access": candidate.access}
		}
		if candidate.procMechanism != "" && procNodes[candidate.to] == nil {
			procNodes[candidate.to] = &graph.Node{
				ID: candidate.to, Kind: graph.KindString, Name: candidate.procMechanism,
				Meta: map[string]any{"context": "process", "mechanism": candidate.procMechanism},
			}
		}
		if !add(candidate.from, candidate.to, candidate.kind, candidate.origin, candidate.file, candidate.line, meta) {
			continue
		}
		switch candidate.kind {
		case graph.EdgeReadsEnv:
			readsEnv++
		case graph.EdgeExecutesProcess:
			execProc++
		case graph.EdgeAccessesField:
			fieldAccess++
		}
	}

	for _, s := range indirectMutationEdges(g) {
		if add(s.from, s.to, graph.EdgeAccessesField, graph.OriginASTInferred, s.file, s.line,
			map[string]any{"access": "write", "indirect": true, "via": s.via}) {
			fieldAccess++
		}
	}

	nodes := make([]*graph.Node, 0, len(procNodes))
	for _, n := range procNodes {
		nodes = append(nodes, n)
	}
	edges := make([]*graph.Edge, 0, len(pending))
	for _, s := range pending {
		edges = append(edges, &graph.Edge{
			From: s.from, To: s.to, Kind: s.kind,
			FilePath: s.file, Line: s.line, Origin: s.origin, Meta: s.meta,
		})
	}
	g.AddBatch(nodes, collapseCapabilityIdentities(edges))
	return readsEnv, execProc, fieldAccess
}
