package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const contractDependencyInputMeta = "contract_dependency_inputs"
const contractDependencyInputVersion = "contract-dependencies-v1"

// Bump when contract extraction or its durable record enrichment changes.
// Parser/post-extraction versions alone do not version contracts' policies.
const contractExtractionPolicyVersion = "contract-extraction-v1"

// This receipt describes accepted extraction inputs, independently of the
// contract-record digest. Graph fingerprints omit constant-value sidecars and
// old source markers, so they cannot replace this receipt.
type contractDependencyInputs struct {
	Version, Config, Language, Source string
	Declarations, Imports, Runtime    string
	Constants                         string
	SharedDeclarations                string
	CrossFile                         bool
}

func (idx *Indexer) stampContractDependencyInputs(relPath, language string, src []byte, result *parser.ExtractionResult) {
	if result == nil || extractionDispositionFor(result).omitSecondarySourceScans() {
		return
	}
	derived := storedDerivedFingerprints(result.Nodes)
	if !derived.complete() {
		return
	}
	configBytes, err := json.Marshal(struct {
		Settings, EventBus                        any
		Extractor                                 string
		Records                                   string
		ContractPolicy                            string
		ExtractorVersion, ExtractionPolicyVersion int
	}{idx.config, idx.eventBusBoundaries(), contractExtractorIdentity(idx, language), contracts.RecordFingerprintVersion, contractExtractionPolicyVersion,
		extractorVersionForLang(language), postExtractionPolicyVersion})
	if err != nil {
		return
	}
	constantFingerprint, ok := contractConstantInputsFingerprint(result.ConstValues)
	if !ok {
		return
	}
	sharedDeclarations, ok := contractSharedDeclarationInputs(result)
	if !ok {
		return
	}
	stamp := contractDependencyInputs{
		Version: contractDependencyInputVersion, Config: contractInputHash(configBytes),
		Language: language, Source: contractInputHash(src), Declarations: derived.declarations,
		Imports: derived.imports, Runtime: derived.runtime, Constants: constantFingerprint,
		CrossFile:          contractSourceNeedsFullRefresh(relPath, language, src),
		SharedDeclarations: sharedDeclarations,
	}
	encoded, err := json.Marshal(stamp)
	if err != nil {
		return
	}
	for _, node := range result.Nodes {
		if node != nil && node.Kind == graph.KindFile {
			if node.Meta == nil {
				node.Meta = make(map[string]any)
			}
			node.Meta[contractDependencyInputMeta] = string(encoded)
		}
	}
}

func contractExtractorIdentity(idx *Indexer, language string) string {
	extractor, _ := idx.registry.GetByLanguage(language)
	return fmt.Sprintf("%T", extractor)
}

func contractInputHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func storedContractDependencyInputs(nodes []*graph.Node) (contractDependencyInputs, bool) {
	var stamp contractDependencyInputs
	found := false
	for _, node := range nodes {
		if node == nil || node.Kind != graph.KindFile {
			continue
		}
		encoded, ok := node.Meta[contractDependencyInputMeta].(string)
		var candidate contractDependencyInputs
		if !ok || json.Unmarshal([]byte(encoded), &candidate) != nil || candidate.Version != contractDependencyInputVersion ||
			candidate.Config == "" || candidate.Source == "" || candidate.Constants == "" || candidate.Declarations == "" ||
			candidate.Imports == "" || candidate.Runtime == "" || candidate.Language == "" || candidate.SharedDeclarations == "" {
			return contractDependencyInputs{}, false
		}
		if found && stamp != candidate {
			return contractDependencyInputs{}, false
		}
		stamp, found = candidate, true
	}
	return stamp, found
}

// Shared type/field and constant declarations can affect contracts extracted
// from other files even when the changed file owns no contract itself. This
// input is separate from function-body/runtime fingerprints.
func contractSharedDeclarationInputs(result *parser.ExtractionResult) (string, bool) {
	rows := make([]fingerprintDigest, 0)
	h := sha256.New()
	for _, node := range result.Nodes {
		if node == nil {
			continue
		}
		switch node.Kind {
		case graph.KindType, graph.KindInterface, "struct", "enum", "class", "trait", graph.KindField, graph.KindConstant, graph.KindEnumMember:
			digest, ok := nodeFingerprintDigest(h, node, fingerprintDerived)
			if !ok {
				return "", false
			}
			rows = append(rows, digest)
		}
	}
	return stableFingerprintDigests(rows), true
}

func sameContractDependencyInputs(prior, fresh []*graph.Node) bool {
	old, oldOK := storedContractDependencyInputs(prior)
	current, currentOK := storedContractDependencyInputs(fresh)
	if !oldOK || !currentOK || old.CrossFile || current.CrossFile {
		return false
	}
	return equalContractDependencyInputs(old, current)
}

func equalContractDependencyInputs(old, current contractDependencyInputs) bool {
	// Source identities bind the receipts to their accepted reads; content is
	// allowed to differ only after all contract dependency inputs agree.
	old.Source, current.Source = "", ""
	return old == current
}

func unchangedProbeContractInputs(prior []*graph.Node, probe fileDeltaProbe) bool {
	old, complete := storedContractDependencyInputs(prior)
	return complete && probe.contractInputsComplete && !old.CrossFile && !probe.contractInputs.CrossFile && equalContractDependencyInputs(old, probe.contractInputs)
}

// verifiedHeadContractInputs reconstructs only the accepted bytes identified by
// the durable file receipt. Declaration identity/span equality is insufficient
// for constants and removed cross-file mounts. Unknown prior content declines.
func (idx *Indexer) verifiedHeadContractInputs(root, sha string) func(*incrementalBatchStage) (contractDependencyInputs, bool) {
	return func(stage *incrementalBatchStage) (contractDependencyInputs, bool) {
		if root == "" || sha == "" || stage == nil || stage.prepared == nil {
			return contractDependencyInputs{}, false
		}
		reader, ok := idx.graph.(graph.ConstantValueProjectionReader)
		if !ok {
			return contractDependencyInputs{}, false
		}
		ctx := idx.contractProjectionContext
		if ctx == nil {
			ctx = context.Background()
		}
		key := graph.ConstantFileKey{RepoPrefix: idx.repoPrefix, FilePath: stage.graphPath}
		projection, err := reader.ReadConstantValueProjectionContext(ctx, nil, []graph.ConstantFileKey{key})
		if err != nil {
			idx.rememberContractInputError(err)
			return contractDependencyInputs{}, false
		}
		row, ok := projection.Files[key]
		if !ok || row.ContentHash == "" {
			return contractDependencyInputs{}, false
		}
		// Do not wait for another admission while retaining the fresh stage.
		// Reserve the accepted prior size before materializing its Git blob.
		if budget := idx.parseAdmission.Load(); budget != nil {
			weight := clampParseWeight(int64(row.Size), budget.capacity)
			if !budget.tryAcquire(weight) {
				return contractDependencyInputs{}, false
			}
			defer (&parseAdmissionLease{shared: budget, sharedWeight: weight}).Release()
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		src, err := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "blob", sha+":"+stage.prepared.relPath).Output()
		if err != nil {
			if ctx.Err() != nil {
				idx.rememberContractInputError(ctx.Err())
			}
			return contractDependencyInputs{}, false
		}
		transformed := idx.transforms.run(stage.prepared.relPath, src)
		if len(transformed) != row.Size || contentHashForSource(transformed) != row.ContentHash {
			return contractDependencyInputs{}, false
		}
		_, _, nodes, valid := idx.extractionFingerprintsOfContentAdmitted(stage.prepared.absPath, src)
		if !valid {
			return contractDependencyInputs{}, false
		}
		receipt, complete := storedContractDependencyInputs(nodes)
		if !complete {
			return contractDependencyInputs{}, false
		}
		var ids []string
		for _, set := range [][]*graph.Node{nodes, stage.priorNodes} {
			for _, node := range set {
				if node != nil {
					ids = append(ids, node.ID)
				}
			}
		}
		actual, err := reader.ReadConstantValueProjectionContext(ctx, appendUniqueSorted(nil, ids...), []graph.ConstantFileKey{key})
		if err != nil {
			idx.rememberContractInputError(err)
			return contractDependencyInputs{}, false
		}
		var constants []parser.ConstValue
		prefix := ""
		if idx.repoPrefix != "" {
			prefix = idx.repoPrefix + "/"
		}
		for _, value := range actual.Rows {
			if value.RepoPrefix == idx.repoPrefix && value.FilePath == stage.graphPath {
				constants = append(constants, parser.ConstValue{NodeID: strings.TrimPrefix(value.NodeID, prefix), FilePath: strings.TrimPrefix(value.FilePath, prefix), Value: value.Value})
			}
		}
		actualFingerprint, valid := contractConstantInputsFingerprint(constants)
		if !valid || receipt.Constants != actualFingerprint {
			return contractDependencyInputs{}, false
		}
		return receipt, true
	}
}

func nodesWithContractInputReceipt(nodes []*graph.Node, stamp contractDependencyInputs) []*graph.Node {
	encoded, err := json.Marshal(stamp)
	if err != nil {
		return nil
	}
	out := append([]*graph.Node(nil), nodes...)
	for i, node := range out {
		if node == nil || node.Kind != graph.KindFile {
			continue
		}
		copyNode := *node
		copyNode.Meta = copyContractMetaMap(node.Meta)
		if copyNode.Meta == nil {
			copyNode.Meta = make(map[string]any)
		}
		copyNode.Meta[contractDependencyInputMeta] = string(encoded)
		out[i] = &copyNode
	}
	return out
}

func contractConstantInputsFingerprint(values []parser.ConstValue) (string, bool) {
	constants := append([]parser.ConstValue(nil), values...)
	sort.Slice(constants, func(i, j int) bool {
		if constants[i].NodeID != constants[j].NodeID {
			return constants[i].NodeID < constants[j].NodeID
		}
		if constants[i].FilePath != constants[j].FilePath {
			return constants[i].FilePath < constants[j].FilePath
		}
		return constants[i].Value < constants[j].Value
	})
	encoded, err := json.Marshal(constants)
	if err != nil {
		return "", false
	}
	return contractInputHash(encoded), true
}
