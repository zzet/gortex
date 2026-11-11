package indexer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

// Build through physical admission and detach, rather than manufacturing any
// proposed accounting metadata. Fixture work is outside the allocation sample.
func checkpointOwnedInventory(t *testing.T, repos, rows int) *contractInventoryValues {
	t.Helper()
	h := sparseShapeStore(t).AtGeneration(10)
	readers := make(map[string]graph.Reader, repos)
	handles := make(map[string][]*store_sqlite.Store, repos)
	for repo := range repos {
		prefix := fmt.Sprintf("r%d", repo)
		files := make([]graph.FileMetaRow, rows)
		for i := range files {
			files[i] = graph.FileMetaRow{
				FilePath:    fmt.Sprintf("%s/f-%04d.go", prefix, i),
				ContentHash: strings.Repeat("owned-hash", 5),
				Errors:      strings.Repeat("owned diagnostic ", 9),
				Size:        i + 1,
				NodeCount:   i + 1,
			}
		}
		require.NoError(t, h.SetFileMetas(prefix, files))
		readers[prefix], handles[prefix] = h, []*store_sqlite.Store{h}
	}
	p := newContractInventoryProjection(readers, handles)
	for prefix := range readers {
		got, err := p.read(t.Context(), prefix)
		require.NoError(t, err)
		require.Len(t, got, rows)
		// The caller's answer is mutable, but cannot alter the retained backing.
		for key := range got {
			delete(got, key)
		}
	}
	values := p.detachedValues()
	require.NotNil(t, values)
	require.Len(t, values.entries, repos)
	for _, entry := range values.entries {
		require.Len(t, entry.rows, rows)
		require.Equal(t, 1, entry.physicalReads)
	}
	return values
}

func checkpointInventoryConvergence(t *testing.T, values *contractInventoryValues) *contractConvergence {
	t.Helper()
	return &contractConvergence{ctx: t.Context(), logger: zap.NewNop(), report: &ContractFollowupReport{}, snap: ContractFollowupSnapshot{inventoryValues: values}}
}

func checkpointInventoryFinish(c *contractConvergence) int64 {
	session := &contractDeltaSession{}
	session.finish(c, false)
	return session.cache.retainedBytes
}

// Compare the actual checkpoint delta to the existing generic full traversal.
// This includes rounded maps, revisions, string backing and any added fields;
// neither the admission estimate nor proposed cache fields are the oracle.
func requireCheckpointInventoryFullCharge(t *testing.T, values *contractInventoryValues) {
	t.Helper()
	withInventory := checkpointInventoryFinish(checkpointInventoryConvergence(t, values))
	withoutInventory := checkpointInventoryFinish(checkpointInventoryConvergence(t, nil))
	expected := contractRetainedSize(reflect.ValueOf(values)) - contractRetainedSize(reflect.ValueOf((*contractInventoryValues)(nil)))
	require.Equal(t, expected, withInventory-withoutInventory, "actual finish must retain the exact full conservative inventory charge")
}

func TestContractInventoryCheckpointUnchangedRowsBoundAllocationWork(t *testing.T) {
	small := checkpointOwnedInventory(t, 1, 1)
	large := checkpointOwnedInventory(t, 1, 256)
	requireCheckpointInventoryFullCharge(t, small)
	requireCheckpointInventoryFullCharge(t, large)
	measure := func(values *contractInventoryValues) float64 {
		c := checkpointInventoryConvergence(t, values)
		var retained int64
		allocations := testing.AllocsPerRun(3, func() {
			retained = checkpointInventoryFinish(c)
		})
		require.Positive(t, retained)
		return allocations
	}
	smallAllocs, largeAllocs := measure(small), measure(large)
	t.Logf("actual finish allocations: one owned row=%.0f, 256 owned rows=%.0f", smallAllocs, largeAllocs)
	// Leave room for constant checkpoint bookkeeping while rejecting a second
	// reflective allocation for every unchanged inventory key and value.
	require.LessOrEqual(t, largeAllocs, smallAllocs+32, "unchanged owned inventory checkpoint accounting must not allocate per retained row")
}

func TestContractInventoryCheckpointFullChargeControls(t *testing.T) {
	t.Run("empty-owned", func(t *testing.T) {
		requireCheckpointInventoryFullCharge(t, checkpointOwnedInventory(t, 1, 0))
	})
	t.Run("multiple-repositories", func(t *testing.T) {
		requireCheckpointInventoryFullCharge(t, checkpointOwnedInventory(t, 2, 9))
	})
	t.Run("manual-unpooled-values", func(t *testing.T) {
		values := &contractInventoryValues{entries: map[string]contractInventoryValueEntry{
			"r": {rows: map[string]graph.FileMetaRow{"r/f.go": {FilePath: "r/f.go", ContentHash: "accepted", Errors: strings.Repeat("owned error ", 4096)}}, bytes: 1},
		}, bytes: 1}
		requireCheckpointInventoryFullCharge(t, values)
	})
}
