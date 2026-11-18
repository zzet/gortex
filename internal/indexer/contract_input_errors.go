package indexer

import (
	"context"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

var errContractInputsChanged = errors.New("selected contract inputs changed")

// Extractor interfaces cannot return store lookup errors. Keep the existing
// extraction API, but retain failures independently so an empty result cannot
// become a proof of unchanged contracts or a successfully published fallback.
type checkedContractEndpointConstants struct {
	indexer *Indexer
	ctx     context.Context
}

var _ contracts.EndpointConstStore = checkedContractEndpointConstants{}

func (s checkedContractEndpointConstants) context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s checkedContractEndpointConstants) FindNodesByNames(names []string) map[string][]*graph.Node {
	rows := make(map[string][]*graph.Node)
	count := 0
	limited := false
	err := graph.VisitNodesByNamesContext(s.context(), s.indexer.graph, names, func(node *graph.Node) bool {
		count++
		if count > graph.ContractProjectionRowLimit {
			limited = true
			return false
		}
		if node != nil {
			rows[node.Name] = append(rows[node.Name], node)
		}
		return true
	})
	if limited {
		err = graph.ErrContractProjectionLimit
	}
	if err != nil {
		s.indexer.rememberContractInputError(err)
		return nil
	}
	return rows
}

func (s checkedContractEndpointConstants) ConstantValuesByNodeIDs(ids []string) (map[string]string, error) {
	rows, err := graph.ConstantValuesByNodeIDsContext(s.context(), s.indexer.graph, ids)
	if err != nil {
		s.indexer.rememberContractInputError(err)
		return nil, err
	}
	return rows, nil
}

func (idx *Indexer) rememberContractInputError(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, graph.ErrConstantProjectionStale) || errors.Is(err, graph.ErrContractProjectionStale) {
		err = fmt.Errorf("%w: %w: %w", ErrDirtySnapshotChanged, errContractInputsChanged, err)
	}
	idx.contractInputErrMu.Lock()
	if idx.contractInputErr == nil {
		idx.contractInputErr = err
	}
	idx.contractInputErrMu.Unlock()
}

func (idx *Indexer) rememberContractReaderStatus() {
	if status, ok := idx.graph.(graph.ConstantValueReadStatus); ok {
		idx.rememberContractInputError(status.ConstantValueReadError())
	}
}

func (idx *Indexer) contractInputError() error {
	idx.rememberContractReaderStatus()
	idx.contractInputErrMu.Lock()
	defer idx.contractInputErrMu.Unlock()
	return idx.contractInputErr
}

func (idx *Indexer) clearContractInputError() {
	idx.contractInputErrMu.Lock()
	idx.contractInputErr = nil
	idx.contractInputErrMu.Unlock()
}
