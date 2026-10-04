package indexer

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Prior fingerprints for rows that carry none.
//
// The derived-pass planner compares a changed file's prior and fresh derived
// fingerprints (declarations, imports, runtime, artifacts) to decide which
// derived families it must redo. A stack whose rows were written before those
// fingerprints were stamped (a store kept across daemon upgrades) has none on
// any file row, and every delta over it fell back to invalidating every
// family ("legacy fallback"), on every save, for as long as the stack lived.
//
// The prior state of a changed file in a delta is the stack's, which is the
// checkout's HEAD content. Its fingerprints are those of HEAD's content,
// parsed exactly as a fresh parse is; the result is used only when the parse
// yields the same node identities and spans as the stack's rows (so the rows
// were derived from that content), and is kept per stack and path.

type priorFingerprints struct {
	graph   fileDeltaFingerprints
	derived derivedFingerprints
}

var editDeltaPriorFingerprints struct {
	sync.Mutex
	entries map[string]priorFingerprints
	order   []string
}

const editDeltaPriorFingerprintEntries = 512

func cachedPriorFingerprints(key string) (priorFingerprints, bool) {
	editDeltaPriorFingerprints.Lock()
	defer editDeltaPriorFingerprints.Unlock()
	fp, ok := editDeltaPriorFingerprints.entries[key]
	return fp, ok
}

func storePriorFingerprints(key string, fp priorFingerprints) {
	editDeltaPriorFingerprints.Lock()
	defer editDeltaPriorFingerprints.Unlock()
	if editDeltaPriorFingerprints.entries == nil {
		editDeltaPriorFingerprints.entries = make(map[string]priorFingerprints)
	}
	if _, ok := editDeltaPriorFingerprints.entries[key]; !ok {
		editDeltaPriorFingerprints.order = append(editDeltaPriorFingerprints.order, key)
	}
	editDeltaPriorFingerprints.entries[key] = fp
	for len(editDeltaPriorFingerprints.order) > editDeltaPriorFingerprintEntries {
		delete(editDeltaPriorFingerprints.entries, editDeltaPriorFingerprints.order[0])
		editDeltaPriorFingerprints.order = editDeltaPriorFingerprints.order[1:]
	}
}

// resetEditDeltaPriorFingerprints empties the cache (tests).
func resetEditDeltaPriorFingerprints() {
	editDeltaPriorFingerprints.Lock()
	editDeltaPriorFingerprints.entries = nil
	editDeltaPriorFingerprints.order = nil
	editDeltaPriorFingerprints.Unlock()
}

// headPriorFingerprints returns the fingerprint source a delta over a stack
// at HEAD commit sha installs: it reads a file's HEAD content with git. key
// is the stack's identity; an empty sha or key installs nothing.
func headPriorFingerprints(idx *Indexer, rootPath, sha, key string) func(absPath string, priorNodes []*graph.Node) (fileDeltaFingerprints, derivedFingerprints, bool) {
	return headPriorFingerprintsExcept(idx, rootPath, sha, key, nil)
}

// headPriorFingerprintsExcept is headPriorFingerprints with the paths unkept
// reports neither read from nor kept in the stack's cache: over a dirty chain
// the cache is the stack's below the chain, and a path the chain speaks for
// has the chain's prior rows, which the HEAD parse is compared with afresh.
func headPriorFingerprintsExcept(idx *Indexer, rootPath, sha, key string, unkept func(rel string) bool) func(absPath string, priorNodes []*graph.Node) (fileDeltaFingerprints, derivedFingerprints, bool) {
	if idx == nil || rootPath == "" || sha == "" || key == "" {
		return nil
	}
	read := func(rel string) ([]byte, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "-C", rootPath, "cat-file", "blob", sha+":"+rel)
		out, err := cmd.Output()
		return out, err == nil
	}
	return func(absPath string, priorNodes []*graph.Node) (fileDeltaFingerprints, derivedFingerprints, bool) {
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(rootPath, absPath)
		}
		rel, err := filepath.Rel(rootPath, absPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		rel = filepath.ToSlash(rel)
		cacheKey := key + "\x00" + rel
		keep := unkept == nil || !unkept(rel)
		store := func(fp priorFingerprints) {
			if keep {
				storePriorFingerprints(cacheKey, fp)
			}
		}
		if fp, ok := cachedPriorFingerprints(cacheKey); ok && keep {
			// A refusal is kept too (empty fingerprints): the stack's rows
			// do not change.
			return fp.graph, fp.derived, fp.derived.complete()
		}
		src, ok := read(rel)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		g, d, nodes, ok := idx.extractionFingerprintsOfContent(absPath, src)
		if !ok || !d.complete() {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		if head, rows := nodeShape(nodes), nodeShape(priorNodes); head != rows {
			if idx.logger != nil {
				idx.logger.Info("edit delta: prior rows are not HEAD's parse; prior fingerprints left unset",
					zap.String("path", rel), zap.Int("head_nodes", len(nodes)), zap.Int("prior_nodes", len(priorNodes)),
					zap.String("first_difference", firstShapeDifference(head, rows)))
			}
			store(priorFingerprints{})
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		store(priorFingerprints{graph: g, derived: d})
		return g, d, true
	}
}

// nodeShape is a node set's declaration, import and file identities with
// their spans: the evidence that two node sets were derived from the same
// content. Locals, parameters and literal nodes are left out: the stored rows
// fold and re-home some of them after the parse (a string literal shared
// across files, a local merged with a same-named one), and they carry none of
// the derived families the fingerprints classify beyond what the declarations
// around them already pin.
func nodeShape(nodes []*graph.Node) string {
	rows := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || n.ID == "" {
			continue
		}
		if n.Kind != graph.KindFile && !isDeclarationNodeKind(n.Kind) && !isImportNodeKind(n.Kind) {
			continue
		}
		rows = append(rows, n.ID+"\x00"+strconv.Itoa(n.StartLine)+"\x00"+strconv.Itoa(n.EndLine))
	}
	sort.Strings(rows)
	out := ""
	for _, r := range rows {
		out += r + "\n"
	}
	return out
}

// extractionFingerprintsOfContent parses src as the file at absPath exactly as
// prepareFileDelta parses the file's current bytes, and returns the
// fingerprints of that parse and its nodes. Nothing is cached or written.
func (idx *Indexer) extractionFingerprintsOfContent(absPath string, src []byte) (fileDeltaFingerprints, derivedFingerprints, []*graph.Node, bool) {
	lease, err := idx.acquireSharedParsePath(absPath)
	if err != nil {
		return fileDeltaFingerprints{}, derivedFingerprints{}, nil, false
	}
	defer lease.Release()
	return idx.extractionFingerprintsOfContentAdmitted(absPath, src)
}

func (idx *Indexer) extractionFingerprintsOfContentAdmitted(absPath string, src []byte) (fileDeltaFingerprints, derivedFingerprints, []*graph.Node, bool) {
	relPath := idx.relKey(absPath)
	lang, ok := idx.effectiveLanguage(absPath, src)
	if !ok {
		return fileDeltaFingerprints{}, derivedFingerprints{}, nil, false
	}
	ext, _ := idx.registry.GetByLanguage(lang)
	if ext == nil {
		return fileDeltaFingerprints{}, derivedFingerprints{}, nil, false
	}
	src = idx.transforms.run(relPath, src)
	var result *parser.ExtractionResult
	var skipped bool
	var err error
	result, skipped, err = idx.extractFileWithRawLease(nil, nil, nil, absPath, relPath, lang, ext, src)
	defer result.ReleaseTree()
	if result == nil || skipped || err != nil {
		return fileDeltaFingerprints{}, derivedFingerprints{}, nil, false
	}
	if !extractionDispositionFor(result).omitSecondarySourceScans() {
		idx.applyCoverageDomains(relPath, lang, src, result)
	}
	g, d, ok := extractionFingerprints(result)
	if !ok {
		return fileDeltaFingerprints{}, derivedFingerprints{}, nil, false
	}
	stampExtractionGraphFingerprints(result, g)
	stampDerivedFingerprints(result, d)
	idx.stampContractDependencyInputs(relPath, lang, src, result)
	nodes := make([]*graph.Node, 0, len(result.Nodes))
	for _, n := range result.Nodes {
		if n == nil {
			continue
		}
		c := *n
		nodes = append(nodes, &c)
	}
	idx.applyRepoPrefix(nodes, nil)
	return g, d, nodes, true
}

// firstShapeDifference names the first row two node shapes disagree on.
func firstShapeDifference(a, b string) string {
	as, bs := strings.Split(a, "\n"), strings.Split(b, "\n")
	seen := make(map[string]struct{}, len(bs))
	for _, row := range bs {
		seen[row] = struct{}{}
	}
	for _, row := range as {
		if _, ok := seen[row]; !ok {
			return "head only: " + strings.ReplaceAll(row, "\x00", " ")
		}
	}
	seen = make(map[string]struct{}, len(as))
	for _, row := range as {
		seen[row] = struct{}{}
	}
	for _, row := range bs {
		if _, ok := seen[row]; !ok {
			return "prior only: " + strings.ReplaceAll(row, "\x00", " ")
		}
	}
	return ""
}
