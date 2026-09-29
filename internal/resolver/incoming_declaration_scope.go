package resolver

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Incoming-leg scoping by declaration surface.
//
// The incremental reverse (incoming) leg re-attempts every unresolved
// reference parked on the name-owned stub keys of the symbols the changed
// files declare. On a real multi-repository workspace a file that declares a
// common name (Load, Config, New) has hundreds of such references parked from
// other files and repositories, almost all of them permanently unresolved
// (a different repository's `Load` that never binds here). Re-attempting them
// on every save is what makes the reverse leg — and the page indexes it needs
// (repository prefixes, directory/provides indexes, import reachability of
// every caller file) — the dominant cost of a one-line body edit.
//
// A parked reference whose binding inputs did not change cannot bind
// differently now. Its inputs are its own source (unchanged: it is not in the
// changed file, or the forward leg re-resolves it) and the candidate set of
// the name it waits on. The changed file contributes to that candidate set
// exactly through the declarations that own the stub key; when those
// declarations are identical before and after the edit, the edit did not
// change the candidate set, and the reference was last attempted against the
// same candidates — the symmetric half of the forward leg's
// prior-unresolved skip (incremental.go), which leaves an unchanged pending
// reference for "the incoming pass when a matching symbol appears".
//
// Two classes of parked reference are still admitted on a carried key:
//
//   - a reference the re-parse restubbed (it carries the restub provenance
//     stash): it was BOUND before the edit, eviction parked it, and only the
//     incoming leg rebinds it;
//   - every reference on a key whose declarations changed, appeared, or whose
//     prior evidence is missing: without evidence nothing is skipped.
//
// The evidence is transaction-scoped: the caller captures the changed files'
// declaration surfaces immediately before it evicts them, installs them with
// SetPriorDeclarations, runs one incremental resolve, and clears them — the
// same lifetime SetIncrementalSkip has. It never outlives the mutation it
// describes, so no later mutation can make it stale.

// EvidenceScopeEnv overrides the configured evidence-scoping switch for the
// whole process: "on" enables it, "off" disables it, anything else (unset
// included) defers to the configuration.
const EvidenceScopeEnv = "GORTEX_RESOLVER_EVIDENCE_SCOPE"

// EvidenceScopingEnabled reports whether incremental resolves may use
// pre-mutation evidence to narrow their work: the incoming leg's declaration
// carry here, and the deferred catch-up's hand-off of the forward leg's
// prior-unresolved skip (internal/indexer). configured is the configuration's
// choice (index.dirty_chain.resolver_evidence_scope; on unless set false).
// The scoped legs are not row-identical to the exhaustive ones, but against a
// clean index of the edited tree they differ only where the exhaustive legs
// differ too. EvidenceScopeEnv overrides the configuration either way. Read
// per call so a test can flip it.
func EvidenceScopingEnabled(configured bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EvidenceScopeEnv))) {
	case "on":
		return true
	case "off":
		return false
	}
	return configured
}

// SetEvidenceScoping records the configuration's evidence-scoping choice for
// this resolver (see EvidenceScopingEnabled). On unless set false.
func (r *Resolver) SetEvidenceScoping(on bool) {
	r.evidenceScopingOff = !on
}

// EvidenceScoping reports whether this resolver's incremental resolves use
// pre-mutation evidence: its configured choice under EvidenceScopeEnv.
func (r *Resolver) EvidenceScoping() bool {
	if r == nil {
		return EvidenceScopingEnabled(true)
	}
	return EvidenceScopingEnabled(!r.evidenceScopingOff)
}

// DeclarationSurface is one file's name-owned declaration evidence: for every
// unresolved-stub key its referenceable declarations own, a digest of those
// declarations over every field resolution reads. Source positions are not
// part of it: a body edit above a declaration shifts its lines without
// changing what can bind to it.
type DeclarationSurface map[string]string

// surfacePositionalMetaKeys are node Meta entries that describe where, how
// large or what shape a declaration's body is, never what can bind to it. They
// change on an ordinary body edit and nothing in resolution reads them; every
// other Meta entry is part of the surface, so an unrecognised key can only make
// the surface compare unequal (the conservative direction).
//
// Besides the size keys this covers the body metrics the extractors stamp
// (parser/languages/helpers_complexity.go) and the body-derived keys the
// indexer's derived fingerprint already sets aside (clone_sig and the body
// texts, indexer/file_delta.go). clone_sig matters most: a whole index stamps
// it on every function, while a freshly extracted declaration does not carry
// it until the clone pass runs, so leaving it in made every declaration of an
// edited file compare changed and re-opened every parked reference on every
// name the file declares — each save of config.go re-attempted, and re-wrote,
// the thousands of `*.Load` references parked across the repository.
var surfacePositionalMetaKeys = map[string]struct{}{
	"complexity": {},
	"loc":        {},
	"lines":      {},
	// body metrics
	"cognitive":           {},
	"loop_depth":          {},
	"max_access_depth":    {},
	"linear_scan_in_loop": {},
	"alloc_in_loop":       {},
	"recursion_in_loop":   {},
	// body-derived texts and signatures
	"clone_sig":   {},
	"body":        {},
	"body_hash":   {},
	"body_text":   {},
	"content":     {},
	"raw_source":  {},
	"snippet":     {},
	"source_text": {},
}

// DeclarationSurfaceOf derives the declaration surface of one file's nodes.
func DeclarationSurfaceOf(nodes []*graph.Node) DeclarationSurface {
	perKey := make(map[string][]string)
	for _, node := range nodes {
		if node == nil || node.Name == "" || !graph.IsReferenceableSymbol(node.Kind) {
			continue
		}
		digest := declarationDigest(node)
		for _, key := range graph.UnresolvedNameCandidateIDs(node) {
			perKey[key] = append(perKey[key], digest)
		}
	}
	surface := make(DeclarationSurface, len(perKey))
	for key, digests := range perKey {
		sort.Strings(digests)
		surface[key] = strings.Join(digests, "\x1e")
	}
	return surface
}

// declarationDigest renders every field of a declaration that candidate
// selection can read. It is a comparison key, not a hash: equal strings mean
// equal declarations, and the rendering is deterministic (encoding/json sorts
// map keys).
func declarationDigest(node *graph.Node) string {
	var b strings.Builder
	for _, part := range []string{
		node.ID, string(node.Kind), node.Name, node.QualName, node.FilePath,
		node.Language, node.RepoPrefix, node.WorkspaceID, node.ProjectID, node.Origin,
	} {
		b.WriteString(part)
		b.WriteByte('\x1f')
	}
	if node.Stub {
		b.WriteString("stub")
	}
	b.WriteByte('\x1f')
	if len(node.Meta) > 0 {
		meta := make(map[string]any, len(node.Meta))
		for key, value := range node.Meta {
			if _, positional := surfacePositionalMetaKeys[key]; positional {
				continue
			}
			meta[key] = value
		}
		encoded, err := json.Marshal(meta)
		if err != nil {
			// An unrenderable value cannot be compared: mark the
			// declaration so the key it owns is never carried.
			return unrenderableDeclaration
		}
		b.Write(encoded)
	}
	return b.String()
}

// unrenderableDeclaration marks a declaration whose Meta could not be
// rendered. A key owning one is never carried (incomingCarryFor).
const unrenderableDeclaration = "\x00unrenderable-declaration\x00"

// SetPriorDeclarations installs the changed files' declaration surfaces as
// they were immediately before the mutation the next incremental resolve
// catches up (nil clears). Keyed by graph file path. Only the incremental
// file entry points (ResolveFileAndIncoming, ResolveFilesAndIncoming) honour
// it; a file without an entry keeps the exhaustive reverse leg. Callers
// install and clear it around one resolve, like SetIncrementalSkip.
func (r *Resolver) SetPriorDeclarations(prior map[string]DeclarationSurface) {
	if len(prior) == 0 {
		r.priorDeclarations = nil
		return
	}
	r.priorDeclarations = prior
}

// InheritIncrementalEvidence installs on r the pre-mutation evidence another
// resolver currently holds (SetPriorDeclarations / SetIncrementalSkip), for a
// caller whose catch-up runs on a different resolver instance than the one the
// evidence was installed on — the multi-repository master resolver resolving a
// repository's mutation. from == nil clears. The maps are shared read-only;
// the caller clears both resolvers when the resolve returns. The configured
// evidence-scoping choice is inherited with it.
func (r *Resolver) InheritIncrementalEvidence(from *Resolver) {
	if from == nil {
		r.priorDeclarations, r.incrementalSkip = nil, nil
		return
	}
	r.priorDeclarations, r.incrementalSkip = from.priorDeclarations, from.incrementalSkip
	r.evidenceScopingOff = from.evidenceScopingOff
}

// incomingCarry is the per-pass set of stub keys whose owning declarations the
// mutation provably left unchanged.
type incomingCarry struct {
	keys map[string]struct{}
	// evidenceFiles counts frontier files that had prior evidence; files
	// counts the frontier.
	evidenceFiles int
	files         int
}

func (c *incomingCarry) carried(key string) bool {
	if c == nil || len(c.keys) == 0 {
		return false
	}
	_, ok := c.keys[key]
	return ok
}

// admits reports whether the incoming leg must re-attempt edge parked on key.
func (c *incomingCarry) admits(key string, edge *graph.Edge) bool {
	if !c.carried(key) {
		return true
	}
	return graph.HasRestubProvenance(edge)
}

// incomingCarryFor derives the carried keys of one frontier. A key is carried
// only when EVERY frontier file that declares it — before or after the edit —
// has prior evidence and its declarations for that key are identical; one
// file without evidence, or with a changed declaration, admits the key's
// whole bucket.
func (r *Resolver) incomingCarryFor(paths []string, nodesByFile map[string][]*graph.Node) *incomingCarry {
	if len(r.priorDeclarations) == 0 || len(paths) == 0 || !r.EvidenceScoping() {
		return nil
	}
	carry := &incomingCarry{keys: make(map[string]struct{}), files: len(paths)}
	refused := make(map[string]struct{})
	for _, path := range paths {
		current := DeclarationSurfaceOf(nodesByFile[path])
		prior, ok := r.priorDeclarations[path]
		if !ok {
			for key := range current {
				refused[key] = struct{}{}
			}
			continue
		}
		carry.evidenceFiles++
		for key, digest := range current {
			if before, had := prior[key]; had && before == digest &&
				!strings.Contains(digest, unrenderableDeclaration) {
				carry.keys[key] = struct{}{}
			} else {
				refused[key] = struct{}{}
			}
		}
		for key := range prior {
			if _, still := current[key]; !still {
				refused[key] = struct{}{}
			}
		}
	}
	for key := range refused {
		delete(carry.keys, key)
	}
	if len(carry.keys) == 0 {
		return nil
	}
	return carry
}

// carriedKeys is the number of stub keys the frontier carried.
func (f incrementalFileFrontier) carriedKeys() int {
	if f.carry == nil {
		return 0
	}
	return len(f.carry.keys)
}

// filter drops, from a materialized incoming batch, the parked references on
// carried keys that the leg does not admit. A nil carry returns the batch
// unchanged (the exhaustive leg); otherwise the input map is not modified.
func (c *incomingCarry) filter(stubKeys []string, inByStub map[string][]*graph.Edge) map[string][]*graph.Edge {
	if c == nil || len(c.keys) == 0 {
		return inByStub
	}
	out := make(map[string][]*graph.Edge, len(inByStub))
	for _, key := range stubKeys {
		edges, ok := inByStub[key]
		if !ok {
			continue
		}
		if !c.carried(key) {
			out[key] = edges
			continue
		}
		var kept []*graph.Edge
		for _, edge := range edges {
			if edge != nil && graph.IsUnresolvedTarget(edge.To) && !c.admits(key, edge) {
				continue
			}
			kept = append(kept, edge)
		}
		if len(kept) > 0 {
			out[key] = kept
		}
	}
	return out
}

// vanishedDeclarationKeys names the stub keys the frontier files owned before
// the mutation (their installed prior declaration surfaces) and own no more: a
// name the edit renamed away or deleted.
//
// The incoming leg enumerates the stub keys of what the files declare now, so
// a reference parked under a vanished name — including the ones the re-parse
// itself restubbed off the dead definition — is not reached by it. A whole
// index attempts every reference once against the tree as it is: it binds such
// a reference to another definition of the name if one exists and otherwise
// leaves it unresolved, with no restub bookkeeping. Enumerating the vanished
// keys gives the per-save leg that same attempt (the receipt name pass does it
// too, but only when the mutation receipt is complete). The keys are never
// carried (incomingCarryFor refuses a key that disappeared), so every reference
// on them is admitted. Without installed evidence there is nothing to name.
func (r *Resolver) vanishedDeclarationKeys(paths []string, nodesByFile map[string][]*graph.Node) []string {
	if len(r.priorDeclarations) == 0 || len(paths) == 0 {
		return nil
	}
	current := make(map[string]struct{})
	for _, path := range paths {
		for key := range DeclarationSurfaceOf(nodesByFile[path]) {
			current[key] = struct{}{}
		}
	}
	var out []string
	for _, path := range paths {
		for key := range r.priorDeclarations[path] {
			if _, still := current[key]; !still {
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out
}
