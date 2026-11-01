package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/crashpool"
)

const (
	sourceSemanticFingerprintMeta = "source_semantic_fingerprint"
	sourceMetadataFingerprintMeta = "source_metadata_fingerprint"
	sourceCoreFingerprintMeta     = "source_core_fingerprint"
	sourceRawEdgesFingerprintMeta = "source_raw_edges_fingerprint"
)

type fileDeltaFingerprints struct {
	rawEdges string
	semantic string
	metadata string
	core     string
}

// preparedExtraction is the parse result produced by the watcher's delta
// probe. A structural edit consumes it in indexFile so the same bytes are not
// parsed twice. src is the transformed source, matching indexFile's input to
// extractFile.
type preparedExtraction struct {
	absPath     string
	relPath     string
	lang        string
	src         []byte
	result      *parser.ExtractionResult
	readVersion fileReadVersion
	// rawSHA256 is gitstate.BlobSHA256 of the bytes read, before the source
	// transforms; set only for a build that proves its reads
	// (Indexer.contentProof).
	rawSHA256         string
	readRepairBlobSHA string
	parseLease        *parseAdmissionLease
	releaseOnce       sync.Once
}

// fileDeltaProbe exposes phase timings and the three delta boundaries used by
// the watcher: metadata-only, artifact-only, and semantic topology.
type fileDeltaProbe struct {
	readErr                error
	contractInputs         contractDependencyInputs
	contractInputsComplete bool
	fingerprints           fileDeltaFingerprints
	derived                derivedFingerprints
	read                   time.Duration
	extract                time.Duration
	coverage               time.Duration
	fingerprintTime        time.Duration
	readVersion            fileReadVersion
	// rawSHA256 is preparedExtraction.rawSHA256 of the probe's read.
	rawSHA256         string
	readRepairBlobSHA string
}

// prepareFileDelta parses the current file once and caches that exact
// extraction for either the bounded refresh or the structural reindex. Cold
// indexing deliberately does not pay this fingerprint cost: an old/missing
// fingerprint gets one conservative structural patch, which stamps the file
// for subsequent edits.
func (idx *Indexer) prepareFileDelta(filePath string) (fileDeltaProbe, bool) {
	probe, ok, _ := idx.prepareFileDeltaWithAdmission(filePath, false)
	return probe, ok
}

// prepareFileDeltaWithAdmission optionally attempts shared admission without
// waiting. Incremental chunks use tryOnly after they retain their first staged
// result: pressure then returns admissionBusy so the caller can commit and
// release the partial chunk before retrying this file. The first file in a
// chunk still waits, guaranteeing progress even when it is larger than the
// entire budget (its weight is clamped to capacity).
func (idx *Indexer) prepareFileDeltaWithAdmission(filePath string, tryOnly bool) (fileDeltaProbe, bool, bool) {
	return idx.prepareFileDeltaWithAdmissionObserved(filePath, tryOnly, nil)
}

func (idx *Indexer) prepareFileDeltaWithAdmissionObserved(filePath string, tryOnly bool, repair *ReadRepairObservation) (fileDeltaProbe, bool, bool) {
	var probe fileDeltaProbe
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return probe, false, false
	}
	var parseLease *parseAdmissionLease
	if tryOnly {
		var admitted bool
		parseLease, admitted = idx.tryAcquireSharedParsePath(absPath)
		if !admitted {
			return probe, false, true
		}
	} else {
		parseLease, err = idx.acquireSharedParsePath(absPath)
		if err != nil {
			probe.readErr = err
			return probe, false, false
		}
	}
	var (
		result  *parser.ExtractionResult
		skipped bool
	)
	stored := false
	defer func() {
		if !stored {
			result.ReleaseTree()
			parseLease.Release()
		}
	}()
	// relKey: this path is stamped onto node IDs by the extractor below
	// and becomes the graph key at prefixPath(), so it must be the same
	// slash-separated spelling the cold walk stamped. A native-separator
	// key would mint a second node for the same file on Windows rather
	// than an update of the first.
	relPath := idx.relKey(absPath)

	started := time.Now()
	src, readVersion, err := idx.readFileWithVersion(absPath)
	probe.read = time.Since(started)
	if err != nil {
		probe.readErr = contractContentFailure(err)
		return probe, false, false
	}
	if idx.contentProof.active() {
		probe.rawSHA256 = gitstate.BlobSHA256(src)
	}
	lang, ok := idx.effectiveLanguage(absPath, src)
	if !ok {
		return probe, false, false
	}
	ext, _ := idx.registry.GetByLanguage(lang)
	if ext == nil {
		return probe, false, false
	}
	if maxSize := idx.config.MaxFileSize; maxSize > 0 && int64(len(src)) > maxSize {
		return probe, false, false
	}
	if _, skip := idx.newContentAdmissionGate().skip(lang, int64(len(src))); skip {
		return probe, false, false
	}
	if repair != nil && relPath == repair.path {
		probe.readRepairBlobSHA = readRepairBlobSHA(src)
	}
	src = idx.transforms.run(relPath, src)

	var pool *crashpool.Pool
	var quarantine *crashpool.Quarantine
	if idx.crashIsolationEnabled() {
		pool, quarantine = idx.sharedParsePool()
	}
	started = time.Now()
	result, skipped, err = idx.extractFileWithRawLease(
		parseLease, pool, quarantine, absPath, relPath, lang, ext, src,
	)
	probe.extract = time.Since(started)
	if quarantine != nil && quarantine.Len() > 0 {
		_ = quarantine.Save()
	}
	// Ownership of the result's tree-sitter tree, source bytes, and shared
	// admission lease passes to idx.prepared only on the success path below.
	// The defer releases all three resources on every rejected probe.
	if result == nil || skipped || err != nil {
		return probe, false, false
	}

	if !extractionDispositionFor(result).omitSecondarySourceScans() {
		started = time.Now()
		idx.applyCoverageDomains(relPath, lang, src, result)
		probe.coverage = time.Since(started)
	}

	started = time.Now()
	fingerprints, derived, ok := extractionFingerprints(result)
	if !ok {
		return probe, false, false
	}
	probe.fingerprints = fingerprints
	probe.derived = derived
	stampExtractionGraphFingerprints(result, fingerprints)
	stampDerivedFingerprints(result, derived)
	idx.stampContractDependencyInputs(relPath, lang, src, result)
	probe.contractInputs, probe.contractInputsComplete = storedContractDependencyInputs(result.Nodes)
	probe.fingerprintTime = time.Since(started)

	idx.preparedMu.Lock()
	if idx.prepared == nil {
		idx.prepared = make(map[string]*preparedExtraction)
	}
	// A second prepare for the same path before the first was consumed
	// displaces the earlier entry; it owns a tree that nothing else can
	// reach once the map slot is overwritten.
	displaced := idx.prepared[absPath]
	idx.prepared[absPath] = &preparedExtraction{
		absPath:           absPath,
		relPath:           relPath,
		lang:              lang,
		src:               append([]byte(nil), src...),
		result:            result,
		readVersion:       readVersion,
		rawSHA256:         probe.rawSHA256,
		readRepairBlobSHA: probe.readRepairBlobSHA,
		parseLease:        parseLease,
	}
	idx.preparedMu.Unlock()
	// Released outside the lock: release closes the tree through cgo and
	// returns the displaced entry's shared admission capacity.
	displaced.release()
	stored = true
	probe.readVersion = readVersion
	return probe, true, false
}

// release drops every resource owned by a prepared extraction. It is nil-safe
// and idempotent because prepared entries can be released by a deferred batch
// cleanup after an explicit post-commit release.
func (p *preparedExtraction) release() {
	if p == nil {
		return
	}
	p.releaseOnce.Do(func() {
		p.result.ReleaseTree()
		p.parseLease.Release()
	})
}

func (idx *Indexer) takePreparedExtraction(absPath, relPath, lang string, src []byte) (*preparedExtraction, bool) {
	idx.preparedMu.Lock()
	prepared := idx.prepared[absPath]
	delete(idx.prepared, absPath)
	idx.preparedMu.Unlock()
	if prepared == nil || prepared.relPath != relPath || prepared.lang != lang || !bytes.Equal(prepared.src, src) {
		// The entry is already out of the map and the caller gets nothing,
		// so this is the last reference to its retained resources.
		prepared.release()
		return nil, false
	}
	return prepared, true
}

// takePreparedSnapshot consumes the speculative parse without re-reading disk.
// Direct IndexFile uses this to commit the exact accepted read and let its
// original receipt report a concurrent write. Batch refreshes use
// takePreparedRefresh below, which replaces a stale speculative parse instead.
func (idx *Indexer) takePreparedSnapshot(filePath string) (*preparedExtraction, bool) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, false
	}
	idx.preparedMu.Lock()
	prepared := idx.prepared[absPath]
	delete(idx.prepared, absPath)
	idx.preparedMu.Unlock()
	return prepared, prepared != nil
}

func (idx *Indexer) takePreparedRefresh(filePath string) (*preparedExtraction, bool) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, false
	}
	prepared, ok := idx.takePreparedSnapshot(absPath)
	if !ok {
		return nil, false
	}
	current, readVersion, err := idx.readFileWithVersion(absPath)
	if err != nil {
		prepared.release()
		return nil, false
	}
	current = idx.transforms.run(prepared.relPath, current)
	if !bytes.Equal(current, prepared.src) {
		// The file moved on since the speculative parse; the entry is
		// already out of the map, so drop all retained resources with it.
		prepared.release()
		return nil, false
	}
	prepared.readVersion = readVersion
	return prepared, true
}

func (idx *Indexer) discardPreparedExtraction(filePath string) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return
	}
	idx.preparedMu.Lock()
	discarded := idx.prepared[absPath]
	delete(idx.prepared, absPath)
	idx.preparedMu.Unlock()
	// Released outside the lock: release closes the tree through cgo.
	discarded.release()
}

type fingerprintMode uint8

const (
	fingerprintMetadata fingerprintMode = iota
	fingerprintSemantic
	fingerprintCore
	fingerprintDerived
)

var presentationMetaKeys = map[string]struct{}{
	"body": {}, "comment": {}, "comments": {}, "doc": {},
	"documentation": {}, "search_doc": {}, "section_text": {},
	"snippet": {}, "source": {}, "source_text": {},
}

func isFingerprintMeta(key string) bool {
	switch key {
	case sourceSemanticFingerprintMeta, sourceMetadataFingerprintMeta, sourceCoreFingerprintMeta, sourceRawEdgesFingerprintMeta,
		contractDependencyInputMeta,
		sourceDerivedDeclFingerprintMeta, sourceDerivedImportFingerprintMeta,
		sourceDerivedRuntimeFingerprintMeta, sourceDerivedArtifactFingerprintMeta,
		sourceDerivedHierarchyFingerprintMeta:
		return true
	default:
		return false
	}
}

func isArtifactNodeKind(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindArtifact, graph.KindDoc, graph.KindLicense, graph.KindRelease, graph.KindTeam, graph.KindTodo:
		return true
	default:
		return false
	}
}

func fingerprintMetaKeys(meta map[string]any, mode fingerprintMode, keepPresentation bool) []string {
	if len(meta) == 0 {
		return nil
	}
	keys := make([]string, 0, len(meta))
	for key := range meta {
		// The body identity is presentation to the semantic and derived
		// digests, but the metadata digest carries it: an edit inside a body
		// too short to shingle (a comment, a literal) moves nothing else, and
		// an inert verdict would keep the prior row's identity.
		if isFingerprintMeta(key) || key == cloneBodyMetaKey && mode != fingerprintMetadata {
			continue
		}
		if mode != fingerprintMetadata && (!keepPresentation || mode == fingerprintDerived) {
			if _, presentation := presentationMetaKeys[key]; presentation {
				continue
			}
		}
		if mode == fingerprintDerived {
			switch strings.ToLower(key) {
			case "body", "body_hash", "body_text", "clone_sig", "content", "raw_source", "snippet", "source_text":
				continue
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// semanticFingerprintCoversDerived reports whether the semantic row digest is
// also a valid derived-work digest. Most extracted rows have no metadata, so
// reusing that digest removes a third full field/hash pass from cold indexing.
func semanticFingerprintCoversDerived(meta map[string]any, keepPresentation bool) bool {
	for key := range meta {
		if isFingerprintMeta(key) || key == cloneBodyMetaKey {
			continue
		}
		if _, presentation := presentationMetaKeys[key]; presentation {
			if keepPresentation {
				return false
			}
			continue
		}
		switch strings.ToLower(key) {
		case "body", "body_hash", "body_text", "clone_sig", "content", "raw_source", "snippet", "source_text":
			return false
		}
	}
	return true
}

type fingerprintDigest [sha256.Size]byte

func writeFingerprintUint(h hash.Hash, value uint64) {
	var scratch [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(scratch[:], value)
	_, _ = h.Write(scratch[:n])
}

func writeFingerprintInt(h hash.Hash, value int) {
	var scratch [binary.MaxVarintLen64]byte
	n := binary.PutVarint(scratch[:], int64(value))
	_, _ = h.Write(scratch[:n])
}

func writeFingerprintBytes(h hash.Hash, value []byte) {
	writeFingerprintUint(h, uint64(len(value)))
	_, _ = h.Write(value)
}

func writeFingerprintString(h hash.Hash, value string) {
	writeFingerprintUint(h, uint64(len(value)))
	_, _ = h.Write([]byte(value))
}

func writeFingerprintBool(h hash.Hash, value bool) {
	if value {
		_, _ = h.Write([]byte{1})
		return
	}
	_, _ = h.Write([]byte{0})
}

func writeFingerprintMeta(h hash.Hash, meta map[string]any, mode fingerprintMode, keepPresentation bool) bool {
	keys := fingerprintMetaKeys(meta, mode, keepPresentation)
	writeFingerprintUint(h, uint64(len(keys)))
	for _, key := range keys {
		encoded, err := json.Marshal(meta[key])
		if err != nil {
			return false
		}
		writeFingerprintString(h, key)
		writeFingerprintBytes(h, encoded)
	}
	return true
}

func nodeFingerprintDigest(h hash.Hash, node *graph.Node, mode fingerprintMode) (fingerprintDigest, bool) {
	h.Reset()
	_, _ = h.Write([]byte{'N'})
	writeFingerprintString(h, node.ID)
	writeFingerprintString(h, string(node.Kind))
	writeFingerprintString(h, node.Name)
	writeFingerprintString(h, node.QualName)
	writeFingerprintString(h, node.FilePath)
	if mode == fingerprintMetadata {
		writeFingerprintInt(h, node.StartLine)
		writeFingerprintInt(h, node.EndLine)
		writeFingerprintInt(h, node.StartColumn)
		writeFingerprintInt(h, node.EndColumn)
	} else {
		writeFingerprintInt(h, 0)
		writeFingerprintInt(h, 0)
		writeFingerprintInt(h, 0)
		writeFingerprintInt(h, 0)
	}
	writeFingerprintString(h, node.Language)
	if !writeFingerprintMeta(h, node.Meta, mode, isArtifactNodeKind(node.Kind)) {
		return fingerprintDigest{}, false
	}
	writeFingerprintString(h, node.RepoPrefix)
	writeFingerprintString(h, node.WorkspaceID)
	writeFingerprintString(h, node.ProjectID)
	writeFingerprintString(h, node.AbsoluteFilePath)
	writeFingerprintString(h, node.Origin)
	writeFingerprintBool(h, node.Stub)
	fetchedAt, err := node.FetchedAt.MarshalJSON()
	if err != nil {
		return fingerprintDigest{}, false
	}
	writeFingerprintBytes(h, fetchedAt)
	var digest fingerprintDigest
	h.Sum(digest[:0])
	return digest, true
}

func edgeFingerprintDigest(h hash.Hash, edge *graph.Edge, mode fingerprintMode) (fingerprintDigest, bool) {
	if math.IsNaN(edge.Confidence) || math.IsInf(edge.Confidence, 0) {
		return fingerprintDigest{}, false
	}
	h.Reset()
	_, _ = h.Write([]byte{'E'})
	writeFingerprintString(h, edge.From)
	writeFingerprintString(h, edge.To)
	writeFingerprintString(h, string(edge.Kind))
	writeFingerprintString(h, edge.FilePath)
	if mode == fingerprintMetadata {
		writeFingerprintInt(h, edge.Line)
	} else {
		writeFingerprintInt(h, 0)
	}
	writeFingerprintUint(h, math.Float64bits(edge.Confidence))
	writeFingerprintString(h, edge.ConfidenceLabel)
	writeFingerprintString(h, edge.Origin)
	writeFingerprintString(h, edge.Tier)
	writeFingerprintBool(h, edge.CrossRepo)
	writeFingerprintString(h, edge.Alias)
	if !writeFingerprintMeta(h, edge.Meta, mode, false) {
		return fingerprintDigest{}, false
	}
	var digest fingerprintDigest
	h.Sum(digest[:0])
	return digest, true
}

func stableFingerprintDigests(rows []fingerprintDigest) string {
	sort.Slice(rows, func(i, j int) bool {
		return bytes.Compare(rows[i][:], rows[j][:]) < 0
	})
	h := sha256.New()
	for _, row := range rows {
		_, _ = h.Write(row[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func extractionFingerprints(result *parser.ExtractionResult) (fileDeltaFingerprints, derivedFingerprints, bool) {
	if result == nil {
		return fileDeltaFingerprints{}, derivedFingerprints{}, false
	}
	rawNodeIDs := make(map[string]struct{}, len(result.Nodes))
	rawFilePath := ""
	fileAnchors := 0
	for _, node := range result.Nodes {
		if node != nil {
			rawNodeIDs[node.ID] = struct{}{}
			if node.Kind == graph.KindFile {
				rawFilePath = node.FilePath
				fileAnchors++
			}
		}
	}
	var artifactIDs map[string]struct{}
	for _, node := range result.Nodes {
		if node == nil || !isArtifactNodeKind(node.Kind) {
			continue
		}
		if artifactIDs == nil {
			artifactIDs = make(map[string]struct{})
		}
		artifactIDs[node.ID] = struct{}{}
	}

	capacity := len(result.Nodes) + len(result.Edges)
	metadataRows := make([]fingerprintDigest, 0, capacity)
	semanticRows := make([]fingerprintDigest, 0, capacity)
	coreRows := make([]fingerprintDigest, 0, capacity)
	var declarations, imports, runtimeRows, artifacts, hierarchy []fingerprintDigest
	h := sha256.New()
	rawEdgesSupported := fileAnchors == 1 && rawFilePath != ""
	rawEdges := sha256.New()
	writeFingerprintString(rawEdges, "raw-edge-vector-v1")
	writeFingerprintUint(rawEdges, uint64(len(result.Edges)))
	for _, node := range result.Nodes {
		if node == nil {
			continue
		}
		metadata, ok := nodeFingerprintDigest(h, node, fingerprintMetadata)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		semantic, ok := nodeFingerprintDigest(h, node, fingerprintSemantic)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		artifact := false
		if _, artifact = artifactIDs[node.ID]; !artifact {
			coreRows = append(coreRows, semantic)
		}
		derived := semantic
		if !semanticFingerprintCoversDerived(node.Meta, artifact) {
			derived, ok = nodeFingerprintDigest(h, node, fingerprintDerived)
			if !ok {
				return fileDeltaFingerprints{}, derivedFingerprints{}, false
			}
		}
		metadataRows = append(metadataRows, metadata)
		semanticRows = append(semanticRows, semantic)
		if isDeclarationNodeKind(node.Kind) {
			declarations = append(declarations, derived)
		}
		if isHierarchyNodeKind(node.Kind) {
			hierarchy = append(hierarchy, derived)
		}
		if isImportNodeKind(node.Kind) {
			imports = append(imports, derived)
		}
		if artifact {
			artifacts = append(artifacts, derived)
		}
	}
	for _, edge := range result.Edges {
		writeFingerprintBool(rawEdges, edge != nil)
		if edge == nil {
			continue
		}
		// Preserve the established source ownership rule. A pathless pseudo
		// source cannot be attributed by either source-node adjacency or the
		// captured by-file sibling dependency guard.
		if edge.FilePath != "" && edge.FilePath != rawFilePath {
			rawEdgesSupported = false
		}
		if edge.FilePath == "" {
			if _, owned := rawNodeIDs[edge.From]; !owned {
				rawEdgesSupported = false
			}
		}
		// Reserved stamps are not part of the established edge digest.
		// They are not expected on raw edges: refuse this extra proof rather
		// than silently declaring an excluded raw payload unchanged.
		for key := range edge.Meta {
			if isFingerprintMeta(key) {
				rawEdgesSupported = false
			}
		}
		metadata, ok := edgeFingerprintDigest(h, edge, fingerprintMetadata)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		// Preserve raw extraction order and every Edge field, including the
		// response-only fields omitted by the established metadata digest.
		// Conflicting duplicate storage keys can have order-dependent winners.
		_, _ = rawEdges.Write(metadata[:])
		writeFingerprintString(rawEdges, edge.Context)
		writeFingerprintString(rawEdges, edge.ReturnUsage)
		writeFingerprintString(rawEdges, edge.Via)
		writeFingerprintBool(rawEdges, edge.NameOnly)
		semantic, ok := edgeFingerprintDigest(h, edge, fingerprintSemantic)
		if !ok {
			return fileDeltaFingerprints{}, derivedFingerprints{}, false
		}
		_, fromArtifact := artifactIDs[edge.From]
		_, toArtifact := artifactIDs[edge.To]
		if !fromArtifact && !toArtifact {
			coreRows = append(coreRows, semantic)
		}
		derived := semantic
		if !semanticFingerprintCoversDerived(edge.Meta, false) {
			derived, ok = edgeFingerprintDigest(h, edge, fingerprintDerived)
			if !ok {
				return fileDeltaFingerprints{}, derivedFingerprints{}, false
			}
		}
		metadataRows = append(metadataRows, metadata)
		semanticRows = append(semanticRows, semantic)
		if isDeclarationEdgeKind(edge.Kind) {
			declarations = append(declarations, derived)
			hierarchy = append(hierarchy, derived)
		}
		if isImportEdgeKind(edge.Kind) {
			imports = append(imports, derived)
		}
		if isRuntimeDerivedEdgeKind(edge.Kind) {
			runtimeRows = append(runtimeRows, derived)
		}
		if fromArtifact || toArtifact {
			artifacts = append(artifacts, derived)
		}
	}
	rawProof := ""
	if rawEdgesSupported {
		rawProof = "v1:" + hex.EncodeToString(rawEdges.Sum(nil))
	}
	return fileDeltaFingerprints{
		rawEdges: rawProof,
		metadata: stableFingerprintDigests(metadataRows),
		semantic: stableFingerprintDigests(semanticRows),
		core:     stableFingerprintDigests(coreRows),
	}, derivedFingerprints{
		declarations: stableFingerprintDigests(declarations),
		imports:      stableFingerprintDigests(imports),
		runtime:      stableFingerprintDigests(runtimeRows),
		artifacts:    stableFingerprintDigests(artifacts),
		hierarchy:    stableFingerprintDigests(hierarchy),
	}, true
}

func stampExtractionGraphFingerprints(result *parser.ExtractionResult, fingerprints fileDeltaFingerprints) {
	for _, n := range result.Nodes {
		if n == nil || n.Kind != graph.KindFile {
			continue
		}
		if n.Meta == nil {
			n.Meta = make(map[string]any)
		}
		n.Meta[sourceSemanticFingerprintMeta] = fingerprints.semantic
		n.Meta[sourceMetadataFingerprintMeta] = fingerprints.metadata
		n.Meta[sourceCoreFingerprintMeta] = fingerprints.core
		if fingerprints.rawEdges != "" {
			n.Meta[sourceRawEdgesFingerprintMeta] = fingerprints.rawEdges
		} else {
			delete(n.Meta, sourceRawEdgesFingerprintMeta)
		}
		return
	}
}

func stampExtractionGraphFingerprint(result *parser.ExtractionResult) string {
	fingerprints, derived, ok := extractionFingerprints(result)
	if !ok {
		return ""
	}
	stampExtractionGraphFingerprints(result, fingerprints)
	stampDerivedFingerprints(result, derived)
	return fingerprints.metadata
}

func storedExtractionGraphFingerprints(nodes []*graph.Node) fileDeltaFingerprints {
	for _, n := range nodes {
		if n == nil || n.Kind != graph.KindFile || n.Meta == nil {
			continue
		}
		semantic, _ := n.Meta[sourceSemanticFingerprintMeta].(string)
		metadata, _ := n.Meta[sourceMetadataFingerprintMeta].(string)
		core, _ := n.Meta[sourceCoreFingerprintMeta].(string)
		rawEdges, _ := n.Meta[sourceRawEdgesFingerprintMeta].(string)
		return fileDeltaFingerprints{semantic: semantic, metadata: metadata, core: core, rawEdges: rawEdges}
	}
	return fileDeltaFingerprints{}
}

// unchangedRawEdgesAndSpans proves the raw extraction supplied exactly the
// same ordered edge vector that produced the stored resolved graph. This is
// not a resolver reconstruction: all resolved/derived rows stay untouched.
// Symbol spans are part of derived edge presentation (clones/frameworks), so
// only the file anchor may change its end position, as with an EOF comment.
func unchangedRawEdgesAndSpans(prior, fresh []*graph.Node) bool {
	oldProof := storedExtractionGraphFingerprints(prior).rawEdges
	newProof := storedExtractionGraphFingerprints(fresh).rawEdges
	if len(oldProof) != len("v1:")+sha256.Size*2 || !strings.HasPrefix(oldProof, "v1:") || oldProof != newProof {
		return false
	}
	if _, err := hex.DecodeString(oldProof[len("v1:"):]); err != nil {
		return false
	}
	oldByID := make(map[string]*graph.Node, len(prior))
	for _, node := range prior {
		if node == nil || oldByID[node.ID] != nil {
			return false
		}
		oldByID[node.ID] = node
	}
	if len(oldByID) != len(fresh) {
		return false
	}
	seen := make(map[string]bool, len(fresh))
	for _, node := range fresh {
		if node == nil || seen[node.ID] {
			return false
		}
		seen[node.ID] = true
		old := oldByID[node.ID]
		if old == nil || old.Kind != node.Kind || old.StartLine != node.StartLine || old.StartColumn != node.StartColumn {
			return false
		}
		if node.Kind != graph.KindFile && (old.EndLine != node.EndLine || old.EndColumn != node.EndColumn) {
			return false
		}
	}
	return true
}

// rawEdgesPriorCompleted is captured before staging the next mutation. The
// raw marker alone is only extraction identity: it is durable before resolver
// and derived catch-up. The installed runtime must certify completion of the
// exact physical current source, through acceptance or its unchanged settled
// write sequence. Never substitute Previous for a pending current prefix.
func (idx *Indexer) rawEdgesPriorCompleted(graphPath string, prior []*graph.Node) bool {
	if idx == nil || (idx.contractCoreRuntime.Load() == nil && idx.contractCoreInputs == nil) {
		return false
	}
	physical, ok := idx.graph.(*store_sqlite.Store)
	if !ok || physical.ViewGeneration() != 0 {
		return false
	}
	input, known := storedContractDependencyInputs(prior)
	if !known {
		return false
	}
	ctx := context.Background()
	if idx.contractCoreInputs != nil {
		ctx = idx.contractCoreInputs.ctx
	}
	before := physical.ContractReceiptWriteStates(idx.repoPrefix, "", []string{graphPath})[graphPath]
	row, found, err := physical.ContractBoundaryReceiptContext(ctx, idx.repoPrefix, "", graphPath)
	if err != nil || !found || row == nil || row.Deleted ||
		row.RepoPrefix != idx.repoPrefix || row.CheckoutID != "" || row.FilePath != graphPath ||
		row.Version != contractBoundaryReceiptVersion || row.SourceFingerprint == "" || row.SourceFingerprint != input.Source {
		return false
	}
	if row.Accepted {
		return true
	}
	// A successful watcher tail can lose its generation without accepting the
	// source. Only its exact still-current sequence certifies that its graph
	// is complete. A new begin (even a refused write) or restart loses this
	// authority; the pending receipt itself remains unaccepted.
	if before.Seq == 0 || !before.Settled || before.Writing {
		return false
	}
	after := physical.ContractReceiptWriteStates(idx.repoPrefix, "", []string{graphPath})[graphPath]
	return before == after
}

func mergeRefreshMeta(oldMeta, freshMeta map[string]any) map[string]any {
	merged := make(map[string]any, len(oldMeta)+len(freshMeta))
	for key, value := range oldMeta {
		if isFingerprintMeta(key) {
			continue
		}
		if _, presentation := presentationMetaKeys[key]; presentation {
			continue
		}
		merged[key] = value
	}
	for key, value := range freshMeta {
		merged[key] = value
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

type edgeRefreshKey struct {
	from  string
	kind  graph.EdgeKind
	alias string
}

// pairMetadataEdgeRefreshes matches source-owned edges by resolved target, not
// their relative line ordering. Repeated targets have no stable callsite key,
// so they must use the structural path instead of guessing a correspondence.
func pairMetadataEdgeRefreshes(graphPath string, priorByID map[string]*graph.Node, oldEdges, freshEdges []*graph.Edge) ([]graph.EdgeReindex, bool) {
	type identity struct {
		edgeRefreshKey
		to string
	}
	owned := func(edge *graph.Edge) bool {
		return edge.FilePath == graphPath || edge.FilePath == "" && priorByID[edge.From] != nil
	}
	freshCounts := make(map[edgeRefreshKey]int)
	freshByID := make(map[identity]*graph.Edge)
	for _, edge := range freshEdges {
		if edge == nil {
			continue
		}
		if !owned(edge) {
			return nil, false
		}
		key := edgeRefreshKey{from: edge.From, kind: edge.Kind, alias: edge.Alias}
		id := identity{edgeRefreshKey: key, to: edge.To}
		if freshByID[id] != nil {
			return nil, false
		}
		freshByID[id] = edge
		freshCounts[key]++
	}
	oldCounts := make(map[edgeRefreshKey]int)
	oldByID := make(map[identity]*graph.Edge)
	for _, edge := range oldEdges {
		if edge == nil || !owned(edge) {
			continue
		}
		key := edgeRefreshKey{from: edge.From, kind: edge.Kind, alias: edge.Alias}
		if freshCounts[key] == 0 {
			continue
		}
		oldCounts[key]++
		id := identity{edgeRefreshKey: key, to: edge.To}
		if freshByID[id] == nil {
			continue
		}
		if oldByID[id] != nil {
			return nil, false
		}
		oldByID[id] = edge
	}
	for key, count := range freshCounts {
		if oldCounts[key] != count {
			return nil, false
		}
	}
	updates := make([]graph.EdgeReindex, 0, len(freshByID))
	for _, fresh := range freshEdges {
		if fresh == nil {
			continue
		}
		key := edgeRefreshKey{from: fresh.From, kind: fresh.Kind, alias: fresh.Alias}
		before := oldByID[identity{edgeRefreshKey: key, to: fresh.To}]
		if before == nil {
			return nil, false
		}
		after := *before
		after.FilePath = fresh.FilePath
		after.Line = fresh.Line
		after.Alias = fresh.Alias
		after.Meta = mergeRefreshMeta(before.Meta, fresh.Meta)
		updates = append(updates, graph.EdgeReindex{
			Edge: &after, OldTo: before.To, OldFilePath: before.FilePath,
			OldLine: before.Line, RefreshIdentity: true,
		})
	}
	return updates, true
}

func metadataEdgeRefreshes(g graph.Store, graphPath string, priorNodes, freshNodes []*graph.Node, freshEdges []*graph.Edge) ([]graph.EdgeReindex, bool) {
	if len(freshNodes) == 0 {
		return nil, false
	}
	priorByID := make(map[string]*graph.Node, len(priorNodes))
	ids := make([]string, 0, len(priorNodes))
	for _, n := range priorNodes {
		if n == nil {
			continue
		}
		priorByID[n.ID] = n
		ids = append(ids, n.ID)
	}
	if len(priorByID) != len(freshNodes) {
		return nil, false
	}
	newIDs := make(map[string]struct{}, len(freshNodes))
	for _, n := range freshNodes {
		if n == nil || priorByID[n.ID] == nil {
			return nil, false
		}
		newIDs[n.ID] = struct{}{}
	}

	reuse, _, priorVis := captureIncrementalState(g, graphPath)
	if priorVis != csharpVisibilityStampForNodes(freshNodes) {
		// A using-stamp change re-prices visibility-narrowed resolutions —
		// a metadata-only refresh could keep verdicts the new usings
		// refuse. Bail to the structural path, which re-resolves.
		return nil, false
	}
	applyResolvedOutEdges(g, freshEdges, reuse, newIDs)

	for _, edge := range freshEdges {
		if edge != nil && priorByID[edge.From] == nil {
			return nil, false
		}
	}
	var oldEdges []*graph.Edge
	for _, edges := range graph.OutEdgesForNodes(g, ids) {
		oldEdges = append(oldEdges, edges...)
	}
	return pairMetadataEdgeRefreshes(graphPath, priorByID, oldEdges, freshEdges)
}

// applyPreparedMetadataRefresh updates source-owned node metadata/locations and
// stable edge spans without evicting the file or invoking the resolver. A
// shape mismatch is conservative: the caller falls back to structural reindex.
func (idx *Indexer) applyPreparedMetadataRefresh(filePath string, priorNodes []*graph.Node) ([]*graph.Node, bool, bool) {
	prepared, ok := idx.takePreparedRefresh(filePath)
	if !ok || prepared.result == nil {
		return nil, false, false
	}
	result := prepared.result
	// takePreparedRefresh already removed the entry from idx.prepared,
	// so this function owns the tree. Only Nodes/Edges are read from
	// here on, and every shape-mismatch check below returns early —
	// release on all of them.
	defer prepared.release()
	idx.applyRepoPrefix(result.Nodes, result.Edges)
	graphPath := idx.prefixPath(prepared.relPath)

	priorByID := make(map[string]*graph.Node, len(priorNodes))
	for _, n := range priorNodes {
		if n != nil {
			priorByID[n.ID] = n
		}
	}
	if len(priorByID) != len(result.Nodes) {
		return nil, false, false
	}
	// Authority comes from untouched fresh extraction, before old presentation
	// metadata is merged into the nodes that will be stored.
	preserveRawEdges := unchangedRawEdgesAndSpans(priorNodes, result.Nodes) && idx.rawEdgesPriorCompleted(graphPath, priorNodes)
	for i, fresh := range result.Nodes {
		if fresh == nil {
			return nil, false, false
		}
		old := priorByID[fresh.ID]
		if old == nil {
			return nil, false, false
		}
		cp := *fresh
		cp.Meta = mergeRefreshMeta(old.Meta, fresh.Meta)
		if cp.WorkspaceID == "" {
			cp.WorkspaceID = old.WorkspaceID
		}
		if cp.ProjectID == "" {
			cp.ProjectID = old.ProjectID
		}
		result.Nodes[i] = &cp
	}

	var edgeUpdates []graph.EdgeReindex
	if !preserveRawEdges {
		var ok bool
		edgeUpdates, ok = metadataEdgeRefreshes(idx.graph, graphPath, priorNodes, result.Nodes, result.Edges)
		if !ok {
			return nil, false, false
		}
	} else if csharpVisibilityStampForNodes(priorNodes) != csharpVisibilityStampForNodes(result.Nodes) {
		return nil, false, false
	}
	if idx.contentSearcher() != nil && !idx.replaceContentSections(graphPath, result.Nodes, false) &&
		len(collectContentItems(result.Nodes)) > 0 {
		return nil, false, false
	}
	idx.graph.AddBatch(result.Nodes, nil)
	idx.graph.ReindexEdges(edgeUpdates)
	idx.persistFileMeta(prepared.relPath, prepared.src, result)

	batcher, _ := idx.graph.(graph.SymbolFTSBatchUpserter)
	var ftsItems []graph.SymbolFTSItem
	if batcher != nil {
		ftsItems = make([]graph.SymbolFTSItem, 0, len(result.Nodes))
	}
	for _, n := range result.Nodes {
		if !idx.shouldIndexForSearch(n) {
			continue
		}
		if idx.search != nil {
			idx.search.Remove(n.ID)
			idx.search.Add(n.ID, searchIndexFields(n, idx.projectName)...)
		}
		if batcher != nil {
			ftsItems = append(ftsItems, graph.SymbolFTSItem{
				NodeID: n.ID,
				Tokens: ftsTokensFor(n, idx.projectName),
			})
		}
	}
	if len(ftsItems) > 0 {
		if err := batcher.BatchUpsertSymbolFTS(ftsItems); err != nil {
			return nil, false, false
		}
	}
	fresh := idx.recordFileReadVersion(prepared.relPath, prepared.absPath, prepared.readVersion)
	return idx.graph.GetFileNodes(graphPath), true, fresh
}
