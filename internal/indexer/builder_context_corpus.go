package indexer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// The capabilities a corpus must have for the builder to withhold context and
// prune pathless identities from it.
//
// Every one of them used to be an optional type assertion, skipped silently
// when the corpus lacked it: a wrapper that dropped one method would have
// brought back an orphaned edge, constant value, symbol-FTS or clone row, or
// the whole stub-adjacency leak, with nothing failing. A sparse build now
// refuses such a corpus instead (requireContextCorpus).

// withholdCapability names one required method set.
type withholdCapability struct {
	name string
	has  func(corpus any) bool
}

var withholdCapabilities = []withholdCapability{
	{"RemoveEdgesExact", func(c any) bool {
		_, ok := c.(interface{ RemoveEdgesExact([]*graph.Edge) int })
		return ok
	}},
	{"DeleteConstantValuesByFiles", func(c any) bool {
		_, ok := c.(interface {
			DeleteConstantValuesByFiles(string, []string) error
		})
		return ok
	}},
	{"DeleteCloneShingles", func(c any) bool {
		_, ok := c.(interface{ DeleteCloneShingles([]string) error })
		return ok
	}},
	{"EvictPathlessNodesByIDs", func(c any) bool {
		_, ok := c.(graph.PathlessNodeBatchEvicter)
		return ok
	}},
	{"EvictContractNodesByIDs", func(c any) bool {
		_, ok := c.(graph.ContractNodeBatchEvicter)
		return ok
	}},
	// Symbol FTS is keyed by identity. The in-memory pass corpus keeps none —
	// its rows are derived at the drain from the nodes that survive — so it
	// alone is exempt; every other corpus must be able to delete them.
	{"BatchDeleteSymbolFTS", func(c any) bool {
		if _, inMemory := c.(*graph.Graph); inMemory {
			return true
		}
		_, ok := c.(interface{ BatchDeleteSymbolFTS([]string) error })
		return ok
	}},
}

// missingContextCorpusCapabilities lists, sorted, the required capabilities
// corpus lacks.
func missingContextCorpusCapabilities(corpus any) []string {
	var missing []string
	for _, capability := range withholdCapabilities {
		if !capability.has(corpus) {
			missing = append(missing, capability.name)
		}
	}
	sort.Strings(missing)
	return missing
}

// requireContextCorpus refuses a corpus that lacks a capability the withhold
// and prune steps need.
func requireContextCorpus(corpus any) error {
	if missing := missingContextCorpusCapabilities(corpus); len(missing) > 0 {
		return fmt.Errorf("indexer: the sparse build's corpus (%T) lacks %s; withholding context from it would leave rows behind silently",
			corpus, strings.Join(missing, ", "))
	}
	return nil
}

// purgeIdentitySidecars removes the identity-keyed side rows of ids: symbol
// FTS (a store corpus only) and clone shingles.
func purgeIdentitySidecars(corpus any, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if deleter, ok := corpus.(interface{ BatchDeleteSymbolFTS([]string) error }); ok {
		if err := deleter.BatchDeleteSymbolFTS(ids); err != nil {
			return fmt.Errorf("indexer: withhold generation symbol index: %w", err)
		}
	} else if _, inMemory := corpus.(*graph.Graph); !inMemory {
		return fmt.Errorf("indexer: the corpus (%T) cannot delete symbol FTS rows", corpus)
	}
	deleter, ok := corpus.(interface{ DeleteCloneShingles([]string) error })
	if !ok {
		return fmt.Errorf("indexer: the corpus (%T) cannot delete clone rows", corpus)
	}
	if err := deleter.DeleteCloneShingles(ids); err != nil {
		return fmt.Errorf("indexer: withhold generation clone corpus: %w", err)
	}
	return nil
}
