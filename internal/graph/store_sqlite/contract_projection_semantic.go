package store_sqlite

import (
	"unsafe"

	"github.com/zzet/gortex/internal/graph"
)

// Compiler binding sidecars are inputs even when no node/edge/receipt changes.
// Their clock advances only after commit, under the same existing write gate.
func (s *Store) noteContractSemanticBindingsLocked(repo string, files []string, rows []graph.SemanticBindingType, wholeRepo bool) {
	revision := s.semanticBindingInputCounter(s.viewGen).Add(1)
	h := s.contractProjectionHistory
	if h == nil {
		return
	}
	change := contractProjectionMutation{complete: true}
	if wholeRepo {
		change.domains = []ContractProjectionDomain{{Kind: "semantic_repo", Scope: repo, Key: "*"}}
	} else if uint64(len(files))+uint64(len(rows)) > h.budget/(2*uint64(unsafe.Sizeof(ContractProjectionDomain{}))) {
		change.complete = false
	} else {
		seen := make(map[string]bool)
		add := func(file string) {
			if file == "" {
				wholeRepo = true
				return
			}
			if !seen[file] {
				seen[file] = true
				change.domains = append(change.domains, ContractProjectionDomain{Kind: "semantic_file", Scope: repo, Key: file})
			}
		}
		for _, file := range files {
			add(file)
		}
		// The historical API permits inserted rows outside files. Preserve that
		// behavior and include their actual frontier instead of ignoring it.
		for _, row := range rows {
			add(row.Site.FilePath)
		}
		if wholeRepo {
			change.domains = []ContractProjectionDomain{{Kind: "semantic_repo", Scope: repo, Key: "*"}}
		}
	}
	s.noteContractProjectionBindingMutationLocked(s.viewGen, revision, change)
}

// Eviction can delete binding rows across repository prefixes. Until the
// removed prefixes are captured, no literal repository domain is complete.
func (s *Store) noteContractUnknownSemanticBindingsLocked() {
	revision := s.semanticBindingInputCounter(s.viewGen).Add(1)
	s.noteContractProjectionBindingMutationLocked(s.viewGen, revision, contractProjectionMutation{})
}
