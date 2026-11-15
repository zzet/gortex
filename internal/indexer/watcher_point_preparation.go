package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
)

// Preparation owns only one staged, read-only extraction. The repository lane
// and primary write intent remain held, but the real prior accepted publication
// is not withdrawn until the ORIGINAL complete mutating executor begins. No
// fingerprint, source acceptance, or replacement lease is created here.
//
// The initial bounded seam is uninstrumented ordinary Go modifies. Observed
// publication attempts retain their original phase-accounting/executor path;
// opaque callbacks, snapshots, deletes, manifests, custom transforms and unknown
// admission stay immediately protected by the original raw Begin.
type watcherPointPreparation struct {
	watcher             *Watcher
	path                string
	kind                ChangeKind
	generation          uint64
	receiptChecked      bool
	ctx                 context.Context
	idx                 *Indexer
	before              *pointDuplicateState
	policy              pointPreparationPolicy
	extractor           parser.Extractor // retains the exact identity; never synthesized
	prepared, installed *preparedExtraction
	probe               fileDeltaProbe
	used                bool
}

type pointPreparationPolicy struct {
	config     string
	extractor  string
	registry   *parser.Registry
	transforms *transformPipeline
	options    *parser.ExtractionOptions
}

func capturePointPreparationPolicy(idx *Indexer, path string) (pointPreparationPolicy, parser.Extractor, bool) {
	if idx == nil || idx.registry == nil || idx.crashIsolationEnabled() || filepath.Ext(path) != ".go" {
		return pointPreparationPolicy{}, nil, false
	}
	if idx.transforms != nil {
		for _, transform := range idx.transforms.prePass {
			if _, ok := transform.(csharpPreprocBlankTransform); !ok {
				return pointPreparationPolicy{}, nil, false
			}
		}
		for _, transform := range idx.transforms.transforms {
			switch transform.(type) {
			case utf16DecodeTransform, bomStripTransform:
			default:
				return pointPreparationPolicy{}, nil, false
			}
		}
	}
	extractor, ok := idx.registry.GetByLanguage("go")
	if !ok || extractor == nil {
		return pointPreparationPolicy{}, nil, false
	}
	identity := fmt.Sprintf("%T/%p", extractor, extractor)
	if strings.Contains(identity, "%!") {
		return pointPreparationPolicy{}, nil, false
	}
	raw, err := json.Marshal(struct {
		Settings config.IndexConfig
		Events   any
	}{idx.config, idx.eventBusBoundaries()})
	if err != nil {
		return pointPreparationPolicy{}, nil, false
	}
	return pointPreparationPolicy{string(raw), identity, idx.registry, idx.transforms, idx.extractionOptions.Load()}, extractor, true
}

// The actual language/identity comes from the SAME source read and registry
// resolution the extractor will use; a .go suffix or a separately registered
// Go extractor alone cannot authorize an opaque remapped producer.
func (p *watcherPointPreparation) allowsExtraction(idx *Indexer, language string, extractor parser.Extractor) bool {
	if p == nil || idx != p.idx || language != "go" || extractor == nil {
		return false
	}
	policy, _, known := capturePointPreparationPolicy(idx, p.path)
	return known && policy == p.policy && fmt.Sprintf("%T/%p", extractor, extractor) == p.policy.extractor
}

func (p *watcherPointPreparation) prepare(ctx context.Context) {
	if p == nil || p.watcher == nil || p.kind != ChangeModified || p.receiptChecked || p.generation == 0 ||
		PublicationStampsFrom(ctx) != nil || p.watcher.pointReindexRaw != nil || p.watcher.pointMutationPatch != nil ||
		!p.watcher.generationCurrent(p.path, p.generation) {
		return
	}
	idx := p.watcher.currentMutationIndexer()
	if idx == nil || dependencyManifestPath(filepath.ToSlash(p.path)) || idx.shouldExclude(p.path, idx.rootPath, false) {
		return
	}
	before := idx.capturePointDuplicateState(ctx, p.path)
	if before == nil || p.watcher.indexer == nil || p.watcher.indexer.legacyOutputTarget() != before.owner {
		return
	}
	// Preserve R18's checked duplicate path without another parse/admission.
	if idx.indexedFileReceiptMatches(p.path) {
		return
	}
	policy, extractor, ok := capturePointPreparationPolicy(idx, p.path)
	if !ok {
		return
	}
	p.ctx, p.idx, p.before, p.policy, p.extractor = ctx, idx, before, policy, extractor
	probe, ok, _ := idx.prepareFileDeltaWithPointAdmissionObserved(p.path, false, nil, p)
	if !ok {
		return
	} // original protected executor owns every failure/fallback write
	prepared, ok := idx.takePreparedRefresh(p.path)
	if !ok || prepared == nil {
		prepared.release()
		return
	}
	p.prepared = prepared
	current, _, known := capturePointPreparationPolicy(idx, p.path)
	if !known || current != policy || prepared.lang != "go" || !before.matches(idx.capturePointDuplicateState(ctx, p.path)) {
		p.release()
		return
	}
	probe.readVersion = prepared.readVersion
	p.probe = probe
}

// takeProbe is single-use and transfers the exact retained parse to the
// existing batch consumer. That consumer still calls takePreparedRefresh,
// re-reading bytes/version before commit; a raced file is reparsed only AFTER
// Begin, never stamped from the earlier snapshot. Reject/release before a new
// admission, including an unexpected first file, to avoid budget self-deadlock.
func (p *watcherPointPreparation) takeProbe(idx *Indexer, path string, reads ...*relationsReadFrontier) (fileDeltaProbe, bool) {
	if p == nil || p.used {
		return fileDeltaProbe{}, false
	}
	p.used = true
	policy, _, known := capturePointPreparationPolicy(idx, path)
	if p.prepared == nil || idx != p.idx || path != p.path || !known || policy != p.policy ||
		!p.watcher.generationCurrent(p.path, p.generation) || !p.before.matches(idx.capturePointDuplicateState(p.ctx, path)) {
		p.release()
		return fileDeltaProbe{}, false
	}
	// Preparation precedes the point executor's frontier. Re-read through its
	// actual protected read before either inert or structural consumption; an
	// earlier parse cannot stand in for a later tool admission's source read.
	prepared, ok := idx.refreshPreparedRead(path, p.prepared, reads...)
	if !ok {
		p.release()
		return fileDeltaProbe{}, false
	}
	p.prepared = prepared
	p.probe.readVersion = prepared.readVersion
	idx.preparedMu.Lock()
	if idx.prepared == nil {
		idx.prepared = make(map[string]*preparedExtraction)
	}
	displaced := idx.prepared[path]
	idx.prepared[path] = p.prepared
	p.installed, p.prepared = p.prepared, nil
	idx.preparedMu.Unlock()
	displaced.release()
	return p.probe, true
}

func (p *watcherPointPreparation) release() {
	if p == nil {
		return
	}
	p.prepared.release()
	p.prepared = nil
	if p.installed != nil {
		p.idx.preparedMu.Lock()
		if p.idx.prepared[p.path] == p.installed {
			delete(p.idx.prepared, p.path)
		}
		p.idx.preparedMu.Unlock()
		p.installed.release()
		p.installed = nil
	}
}
