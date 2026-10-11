package indexer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
)

// editDeltaSharedEmitterExtractions bounds how many unchanged files one delta
// parses to find where a shared registry row's kept copy moved. A delta that
// would need more is refused and the working tree is built the old way.
const editDeltaSharedEmitterExtractions = 64

// editDeltaSharedEmitters finds, for every shared registry row whose kept copy
// a changed file stopped emitting, the smallest unchanged file that still
// emits it: a whole index keeps that file's copy (graph.keepSharedNodeCopy).
// The kept copy was the smallest emitter before the edit, so every other
// emitter the edit did not touch sorts after it, and the first file in path
// order that emits the row is the answer. A file is read only when its bytes
// contain the row's text, and parsed only then. Rows no file emits any more
// are left to the payload, which removes them.
func (b *SparseGenerationBuilder) editDeltaSharedEmitters(
	ctx context.Context, req BuildRequest, idx *Indexer, dw *graph.DeltaWriter, plan buildPlan,
) ([]string, error) {
	lost := dw.LostSharedNodes()
	if len(lost) == 0 {
		return nil, nil
	}
	changed := make(map[string]struct{}, len(plan.indexed)+len(plan.deleted))
	for _, rel := range plan.indexed {
		changed[rel] = struct{}{}
	}
	for _, rel := range plan.deleted {
		changed[rel] = struct{}{}
	}
	var candidates []string
	for n := range dw.View().NodesByKind(graph.KindFile) {
		if n == nil || n.RepoPrefix != req.RepoPrefix {
			continue
		}
		graphPath := n.FilePath
		if graphPath == "" {
			graphPath = n.ID
		}
		if dw.CoversPath(graphPath) {
			continue
		}
		rel, owned := builderRelPath(req.RepoPrefix, graphPath)
		if !owned {
			continue
		}
		if _, skip := changed[rel]; skip {
			continue
		}
		candidates = append(candidates, rel)
	}
	sort.Strings(candidates)
	candidates = slices.Compact(candidates)

	prefix := ""
	if req.RepoPrefix != "" {
		prefix = req.RepoPrefix + "/"
	}
	extractions := 0
	emitters := make(map[string]struct{})
	for _, row := range lost {
		needle := row.Name
		if value, ok := row.Meta["value"].(string); ok && value != "" {
			needle = value
		}
		if needle == "" {
			return nil, &editDeltaRefusedError{reason: "shared row " + row.ID + " has no text to find its emitters by"}
		}
		rawID := strings.TrimPrefix(row.ID, prefix)
		for _, rel := range candidates {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			src, ok := editDeltaReadSource(req, rel)
			if !ok || !bytes.Contains(src, []byte(needle)) {
				continue
			}
			if extractions >= editDeltaSharedEmitterExtractions {
				return nil, &editDeltaRefusedError{reason: "shared row " + row.ID + " needs more than the extraction budget to find its emitter"}
			}
			extractions++
			result, err := idx.ExtractSource(ctx, rel, src)
			if err != nil || result == nil {
				continue
			}
			emits := false
			for _, n := range result.Nodes {
				if n != nil && n.ID == rawID && n.Kind == row.Kind {
					emits = true
					break
				}
			}
			result.ReleaseTree()
			if emits {
				emitters[rel] = struct{}{}
				break
			}
		}
	}
	out := make([]string, 0, len(emitters))
	for rel := range emitters {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// editDeltaReadSource reads an unchanged file of the checkout from disk, the
// same bytes the per-save engine parses when it re-derives the file. The
// build's target source is narrowed to the change set, so it cannot serve it.
// A build that proves its reads records the read (buildContentProof), so the
// fence confirms the scanned file too: whether a file emits a row is decided
// by its bytes as much as a parsed file's rows are.
func editDeltaReadSource(req BuildRequest, rel string) ([]byte, bool) {
	if req.RootPath == "" {
		return nil, false
	}
	abs := filepath.Join(req.RootPath, filepath.FromSlash(rel))
	src, err := os.ReadFile(abs)
	if req.contentProof.active() {
		sum := ""
		if err == nil {
			sum = gitstate.BlobSHA256(src)
		}
		req.contentProof.record(abs, sum)
	}
	return src, err == nil
}
