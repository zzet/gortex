package indexer

import (
	"context"
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

type coldConstantContractInput struct {
	path, language string
	source         []byte
	mtime          int64
}

// Observe attempts, not successful resolutions: an empty/ambiguous lookup
// during parsing is still dependent on constants not yet flushed to the store.
type observedEndpointConstants struct {
	contracts.EndpointConstStore
	attempted *bool
}

func (s observedEndpointConstants) FindNodesByNames(names []string) map[string][]*graph.Node {
	*s.attempted = true
	return s.EndpointConstStore.FindNodesByNames(names)
}

func (s observedEndpointConstants) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	*s.attempted = true
	return s.EndpointConstStore.ConstantValuesByNodeIDs(ids)
}

// The caller invokes this after all parse-sidecar batches have flushed and
// before the registry is committed. No repository contract projection is
// needed: only the files that actually attempted constant resolution are read.
func (idx *Indexer) refreshColdConstantContractFiles(ctx context.Context, reg *contracts.Registry, inputs []coldConstantContractInput, byLanguage map[string][]contracts.Extractor) error {
	byPath := make(map[string]coldConstantContractInput, len(inputs))
	for _, input := range inputs {
		byPath[input.path] = input
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for start := 0; start < len(paths); start += contractFrontierReadBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := paths[start:min(start+contractFrontierReadBatchSize, len(paths))]
		nodes, edges := idx.contractGraphFrontier(chunk)
		for _, path := range chunk {
			if err := ctx.Err(); err != nil {
				return err
			}
			input := byPath[path]
			var fileID string
			for _, node := range nodes[path] {
				if node != nil && node.Kind == graph.KindFile {
					fileID = node.ID
					break
				}
			}
			if fileID == "" {
				return fmt.Errorf("indexer: accepted constant-contract file %s lost its graph rows", path)
			}
			tree := contracts.ParseTreeForLang(input.language, input.source)
			fresh := idx.runContractExtractorsForFile(path, input.source, nodes[path], edges[fileID], byLanguage[input.language], tree)
			if tree != nil {
				tree.Release()
			}
			reg.ReplaceFile(path, fresh)
			idx.contractCacheMu.Lock()
			idx.contractCache[path] = &contractCacheEntry{mtimeNano: input.mtime, contracts: fresh}
			idx.contractCacheMu.Unlock()
		}
	}
	return ctx.Err()
}
