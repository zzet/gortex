package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func maskQuantumBudget(t *testing.T) {
	t.Helper()
	first, minimum, maximum, target := chainFoldFirstRows, chainFoldMinRows, chainFoldMaxRows, chainFoldStepTarget
	chainFoldFirstRows, chainFoldMinRows, chainFoldMaxRows = 64, 64, 64
	// A tiny target encourages separate phases. A phase transition may still
	// enter the first mask operation before the clock advances.
	chainFoldStepTarget = time.Nanosecond
	t.Cleanup(func() {
		chainFoldFirstRows, chainFoldMinRows, chainFoldMaxRows, chainFoldStepTarget = first, minimum, maximum, target
	})
}

func reachFoldMasks(t *testing.T, fold *ChainFold, ctx context.Context) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if fold.phase == foldPhaseMasks {
			return
		}
		done, err := fold.Step(ctx)
		require.NoError(t, err)
		require.False(t, done, "end-member work must have its own observable transaction boundary")
	}
	t.Fatal("never reached the member mask phase")
}

// Each empty metadata operation is still a committed quantum. Context-only
// node IDs also count towards the page bound, even though none becomes hidden.
func TestChainFoldMaskStepsBoundEmptyOperationsAndContextIDs(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	var nodes []*graph.Node
	for i := 0; i < 128; i++ {
		nodes = append(nodes, flatNode(fmt.Sprintf("repo/context.go::C%03d", i), "repo/context.go"))
	}
	nodes = append(nodes, flatNode("repo/z.go::Z", "repo/z.go"))
	member := flattenMember(t, store, "empty-mask-operations", nodes, nil, []FileMask{{RepoPrefix: "repo", FilePath: "repo/context.go", Mode: OwnershipContext}}, nil, nil)
	require.NoError(t, store.Catalog().SetViewGenerationState(ctx, member, ViewGenerationReady, ViewGenerationBuilding))
	maskQuantumBudget(t)
	to := reservedGeneration(t, store, "mask-pages")
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: []int64{member}, To: to, Owner: "test"})
	require.NoError(t, err)
	defer func() { _ = fold.Release(ctx) }()
	var operations []string
	previousHook := chainFoldMaskStepHook
	chainFoldMaskStepHook = func(_ context.Context, table string) error {
		operations = append(operations, table)
		return nil
	}
	t.Cleanup(func() { chainFoldMaskStepHook = previousHook })
	reachFoldMasks(t, fold, ctx)
	// The final FTS step may also commit the first mask operation. It must
	// stop there, and every remaining empty operation gets its own step.
	require.LessOrEqual(t, fold.table, 1)
	require.Len(t, operations, fold.table)
	require.Empty(t, fold.hiddenIDs)
	for operation := fold.table; operation < 9; operation++ {
		before := len(operations)
		_, err = fold.Step(ctx)
		require.NoError(t, err)
		require.Equal(t, foldPhaseMasks, fold.phase, "one Step must not consume every empty operation and the full ID census")
		require.Equal(t, operation+1, fold.table)
		require.Len(t, operations, before+1, "one Step commits exactly one mask operation")
		require.Empty(t, fold.hiddenIDs)
	}
	var expectedOperations []string
	for _, operation := range foldMaskOperations {
		expectedOperations = append(expectedOperations, operation.table)
	}
	require.Equal(t, expectedOperations, operations, "all nine operations, including any committed on phase entry, retain their order")
	for page := 0; page < 2; page++ {
		_, err = fold.Step(ctx)
		require.NoError(t, err)
		require.Equal(t, foldPhaseMasks, fold.phase, "context-filtered pages still obey the 64 source-row budget")
		require.Empty(t, fold.hiddenIDs, "member exclusions are committed only after its final ID page")
	}
	_, err = fold.Step(ctx)
	require.NoError(t, err)
	require.Equal(t, foldPhaseSettle, fold.phase)
	require.Equal(t, map[string]struct{}{"repo/z.go::Z": {}}, fold.hiddenIDs)
	runFold(t, fold, nil)
}

func renderMaskOperationRows(t *testing.T, store *Store, id int64) []string {
	t.Helper()
	var result []string
	tx, err := store.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, op := range foldMaskOperations {
		columns, err := generationCopyColumns(context.Background(), tx, op.table)
		require.NoError(t, err)
		var projection []string
		for _, column := range columns {
			if column != viewGenColumnName {
				projection = append(projection, column)
			}
		}
		rows, err := tx.Query("SELECT "+joinMaskTestColumns(projection)+" FROM "+op.table+" WHERE view_gen=? ORDER BY "+joinMaskTestColumns(op.keys), id)
		require.NoError(t, err)
		for rows.Next() {
			values := make([]any, len(projection))
			scan := make([]any, len(projection))
			for i := range values {
				scan[i] = &values[i]
			}
			require.NoError(t, rows.Scan(scan...))
			encoded, err := json.Marshal(values)
			require.NoError(t, err)
			result = append(result, op.table+":"+string(encoded))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return result
}

func joinMaskTestColumns(columns []string) string {
	result := ""
	for i, column := range columns {
		if i > 0 {
			result += ", "
		}
		result += column
	}
	return result
}

// Earlier mask pages survive an actual queued writer's interruption. Only the
// in-flight page rolls back; the source cursor and member exclusions stay put.
func TestChainFoldMaskWriterInterruptionRetainsCommittedPagesAndPrecedence(t *testing.T) {
	store := openCatalogStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bottom := flattenMember(t, store, "mask-bottom", []*graph.Node{flatNode("repo/old.go::Old", "repo/old.go")}, nil, nil, nil, nil)
	var files []FileMask
	for i := 0; i < 130; i++ {
		files = append(files, FileMask{RepoPrefix: "repo", FilePath: fmt.Sprintf("repo/p%03d.go", i), Mode: OwnershipReplace})
	}
	upper := flattenMember(t, store, "mask-upper", []*graph.Node{flatNode("repo/new.go::New", "repo/new.go")}, nil, files, nil, nil)
	old := boundaryStorageReceipt("gone.go", "old")
	old.Accepted = true
	gone := old
	gone.Fingerprint = "gone"
	gone.SourceFingerprint = "gone-source"
	gone.Deleted = true
	gone.Payload = nil
	gone.LookupKeys = nil
	gone.ProducedKeys = nil
	require.NoError(t, store.AtGeneration(bottom).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{old}))
	require.NoError(t, store.AtGeneration(upper).SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{gone}))
	work, other := contractWorkFixture("old"), contractWorkFixture("retained")
	work.OriginGeneration, other.OriginGeneration = bottom, bottom
	require.NoError(t, store.AtGeneration(bottom).SetContractWork(ctx, []graph.ContractWork{work, other}))
	work.State = graph.ContractWorkComplete
	require.NoError(t, store.AtGeneration(upper).SetContractWork(ctx, []graph.ContractWork{work}))
	require.NoError(t, store.AtGeneration(bottom).SetProducerState(ProducerCompleteness{Producer: "resolver", State: ProducerStateIncomplete, Reason: "lower-partial"}))
	require.NoError(t, store.AtGeneration(upper).SetProducerState(ProducerCompleteness{Producer: "resolver", State: ProducerStateComplete}))
	for _, member := range []int64{bottom, upper} {
		require.NoError(t, store.Catalog().SetViewGenerationState(ctx, member, ViewGenerationReady, ViewGenerationBuilding))
	}
	reference := reservedGeneration(t, store, "mask-reference")
	_, err := store.FlattenGenerationChain(ctx, []int64{bottom, upper}, reference)
	require.NoError(t, err)
	maskQuantumBudget(t)
	to := reservedGeneration(t, store, "mask-interrupted")
	fold, err := store.BeginChainFold(ctx, ChainFoldRequest{Chain: []int64{bottom, upper}, To: to, Owner: "test"})
	require.NoError(t, err)
	defer func() { _ = fold.Release(context.Background()) }()
	reachFoldMasks(t, fold, ctx)
	for fold.table < 5 {
		_, err = fold.Step(ctx)
		require.NoError(t, err)
	}
	_, err = fold.Step(ctx)
	require.NoError(t, err)
	require.Equal(t, foldPhaseMasks, fold.phase)
	require.Equal(t, 5, fold.table, "130 ownership rows require several committed pages")
	require.NotNil(t, fold.after)
	before := renderFoldMasks(t, store, to)
	require.Len(t, before, 64)
	cursor := fold.cursor()
	ready := make(chan struct{})
	writerDone := make(chan error, 1)
	var wait time.Duration
	chainFoldMaskStepHook = func(stepCtx context.Context, table string) error {
		if table != "generation_file_masks" {
			return nil
		}
		close(ready)
		<-stepCtx.Done()
		return stepCtx.Err()
	}
	t.Cleanup(func() { chainFoldMaskStepHook = nil })
	go func() {
		select {
		case <-ready:
		case <-ctx.Done():
			writerDone <- ctx.Err()
			return
		}
		started := time.Now()
		writerErr := store.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "mask-writer"})
		wait = time.Since(started)
		writerDone <- writerErr
	}()
	_, err = fold.Step(ctx)
	writerErr := <-writerDone // Join ownership before any fatal assertion.
	chainFoldMaskStepHook = nil
	require.ErrorIs(t, err, ErrChainFoldYielded)
	require.NoError(t, writerErr)
	limit := 20 * time.Millisecond
	if raceDetectorOn {
		limit = 100 * time.Millisecond
	}
	require.Less(t, wait, limit, "a real foreground writer must still interrupt mask work")
	require.Equal(t, cursor, fold.cursor())
	require.Empty(t, fold.hiddenIDs)
	require.Equal(t, before, renderFoldMasks(t, store, to), "only the second page's uncommitted ownership rows roll back")
	runFold(t, fold, nil)
	require.Equal(t, renderMaskOperationRows(t, store, reference), renderMaskOperationRows(t, store, to))
	require.Equal(t, renderGenerationNodes(t, store, reference), renderGenerationNodes(t, store, to))
	require.Equal(t, renderGenerationEdges(t, store, reference), renderGenerationEdges(t, store, to))
	require.Equal(t, renderFoldFTS(t, store, reference), renderFoldFTS(t, store, to))
	got, known, err := store.AtGeneration(to).ContractBoundaryReceiptContext(ctx, "repo", "", old.FilePath)
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, got.Deleted)
	matches, err := store.AtGeneration(to).ContractBoundaryReceiptsForLookupKeysContext(ctx, old.LookupKeys, 10)
	require.NoError(t, err)
	require.Empty(t, matches, "explicit negative upper owner must suppress the lower owner's keys")
	debts, err := store.AtGeneration(to).ContractWorkContext(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []graph.ContractWork{work, other}, debts, "exact upper acknowledgment wins; distinct deleted-path token remains")
	states, err := store.AtGeneration(to).ProducerStates()
	require.NoError(t, err)
	require.Contains(t, states, ProducerCompleteness{Producer: "resolver", State: ProducerStateIncomplete, Reason: "lower-partial"})
}
