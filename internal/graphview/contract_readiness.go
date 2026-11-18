package graphview

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// ContractSelection names the data a contract consumer will actually read.
// Empty selectors require the complete selected repos. Matchers that broaden
// canonical cohorts must broaden this selection too.
type ContractSelection struct {
	RepoPrefixes []string
	FilePaths    []string
	Groups       []graph.ContractWorkGroup
	SymbolIDs    []string
	LookupKeys   []string
}

type ContractReadiness struct {
	State   CapabilityState
	Pending int
}

// ContractReadiness reads the primary corpus when no routed RepoView was
// selected. It still requires explicit per-repo baseline authority.
func (m *Materializer) ContractReadiness(ctx context.Context, selection ContractSelection) (ContractReadiness, error) {
	if m == nil || m.Store == nil {
		return ContractReadiness{State: StateUnavailable}, nil
	}
	return readContractReadiness(ctx, m.Store.AtGeneration(BaseCorpusGeneration), nil, "", selection)
}

// ContractReadiness reads explicit producer debt only on demand. Ordinary
// source, symbol and local caller materialization never reads token history.
func (v *RepoView) ContractReadiness(ctx context.Context, selection ContractSelection) (ContractReadiness, error) {
	if v == nil || v.contractStore == nil {
		return ContractReadiness{State: StateUnavailable}, nil
	}
	if len(selection.RepoPrefixes) == 0 {
		selection.RepoPrefixes = []string{v.ID.RepoPrefix}
	}
	var base *store_sqlite.Store
	if v.ComposesBaseCorpus() {
		base = v.contractStore.AtGeneration(BaseCorpusGeneration)
	}
	handles := make([]*store_sqlite.Store, 0, len(v.sources))
	for _, source := range v.sources {
		handles = append(handles, source.Handle)
	}
	return readContractReadiness(ctx, base, handles, v.ID.RepoPrefix, selection)
}

func readContractReadiness(ctx context.Context, base *store_sqlite.Store, handles []*store_sqlite.Store, producerRepo string, selection ContractSelection) (ContractReadiness, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ContractReadiness{}, err
	}
	if len(selection.RepoPrefixes) == 0 {
		return ContractReadiness{State: StateUnavailable}, nil
	}
	work := make(map[string]graph.ContractWork)
	baseline := make(map[string]bool, len(selection.RepoPrefixes))
	read := func(handle *store_sqlite.Store, shared bool) error {
		var positiveRows []graph.ContractWork
		if !shared {
			var err error
			positiveRows, err = handle.ContractWorkContext(ctx)
			if err != nil {
				return err
			}
		}
		for _, repo := range selection.RepoPrefixes {
			_, exists, err := handle.GetContractStateContext(ctx, repo)
			if err != nil {
				return err
			}
			baseline[repo] = baseline[repo] || exists
			var rows []graph.ContractWork
			if shared {
				// Inherited corpus rows belong to the primary repo, not every
				// checkout that happens to share its namespace.
				rows, err = handle.ContractWorkForScopeContext(ctx, repo, "")
			} else {
				rows = positiveRows
			}
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.RepoPrefix != repo {
					continue
				}
				if err := composeContractWork(work, row); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if base != nil {
		if err := read(base, true); err != nil {
			return ContractReadiness{}, err
		}
	}
	for index, handle := range handles {
		if err := read(handle, false); err != nil {
			return ContractReadiness{}, err
		}
		if index == 0 {
			rows, err := handle.ProducerStates()
			if err != nil {
				return ContractReadiness{}, err
			}
			for _, row := range rows {
				if CapabilityID(row.Producer) != CapContracts {
					continue
				}
				if row.State == store_sqlite.ProducerStateComplete {
					// A full-root producer is repo-specific; never use it to
					// certify companion repositories in a wider matcher.
					if slices.Contains(selection.RepoPrefixes, producerRepo) {
						baseline[producerRepo] = true
					}
				}
				if row.State == store_sqlite.ProducerStateDisabledByConfig {
					return ContractReadiness{State: StateDisabledByConfig}, nil
				}
			}
		}
	}
	out := ContractReadiness{State: StateComplete}
	for _, row := range work {
		if row.State == graph.ContractWorkPending && contractWorkRelevant(row, selection) {
			out.Pending++
		}
	}
	if out.Pending > 0 {
		if err := ctx.Err(); err != nil {
			return ContractReadiness{}, err
		}
		out.State = StateIncomplete
		return out, nil
	}
	for _, repo := range selection.RepoPrefixes {
		if !baseline[repo] {
			out.State = StateUnavailable
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return ContractReadiness{}, err
	}
	return out, nil
}

func composeContractWork(work map[string]graph.ContractWork, row graph.ContractWork) error {
	if previous, exists := work[row.Token]; exists {
		a, b := previous, row
		a.State, b.State = "", ""
		if !reflect.DeepEqual(a, b) || previous.State == graph.ContractWorkComplete && row.State != graph.ContractWorkComplete {
			return fmt.Errorf("contract work token %q changed immutable inputs or withdrew completion", row.Token)
		}
	}
	work[row.Token] = row
	return nil
}

func contractWorkRelevant(row graph.ContractWork, selection ContractSelection) bool {
	if row.Scope.Unknown {
		return true
	}
	for _, cause := range row.Scope.Causes {
		switch cause {
		case "local_boundary_changed", "boundary_removed", "handler_inputs_changed":
		case "shared_constant_changed", "shared_type_changed", "mount_inputs_changed", "manifest_dependency_changed", "extraction_policy_changed":
			// A file selector alone does not describe its dependency cohorts.
			// Even a known shared-input frontier needs the requested groups
			// before an unrelated answer can be certified.
			if len(row.Scope.Groups) == 0 || len(selection.Groups) == 0 {
				return true
			}
		default:
			return true
		}
	}
	if len(selection.FilePaths)+len(selection.Groups)+len(selection.SymbolIDs)+len(selection.LookupKeys) == 0 {
		return true
	}
	if slices.Contains(selection.FilePaths, row.FilePath) {
		return true
	}
	for _, group := range row.Scope.Groups {
		if slices.Contains(selection.Groups, group) {
			return true
		}
	}
	for _, id := range row.Scope.SymbolIDs {
		if slices.Contains(selection.SymbolIDs, id) {
			return true
		}
	}
	for _, key := range row.Scope.LookupKeys {
		if slices.Contains(selection.LookupKeys, key) {
			return true
		}
	}
	// Shared/name dependency work without an explicit bounded cohort cannot
	// establish that an unrelated owned file is unaffected.
	if len(row.Scope.LookupKeys) > 0 && len(selection.LookupKeys) == 0 {
		return true
	}
	if len(row.Scope.SymbolIDs) > 0 && len(selection.SymbolIDs) == 0 {
		return true
	}
	return false
}
