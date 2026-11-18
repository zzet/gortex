package indexer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

func TestContractInventoryCheckpointChargesUnpooledStringBacking(t *testing.T) {
	finish := func(errors string) int64 {
		values := &contractInventoryValues{entries: map[string]contractInventoryValueEntry{
			"r": {rows: map[string]graph.FileMetaRow{"r/f.go": {FilePath: "r/f.go", ContentHash: "accepted", Errors: errors}}},
		}}
		c := &contractConvergence{ctx: t.Context(), logger: zap.NewNop(), report: &ContractFollowupReport{}, snap: ContractFollowupSnapshot{inventoryValues: values}}
		session := &contractDeltaSession{}
		session.finish(c, false)
		require.NotNil(t, session.cache)
		return session.cache.retainedBytes
	}
	small := finish("")
	data := strings.Repeat("owned-error", 1<<16)
	large := finish(data)
	require.GreaterOrEqual(t, large-small, int64(len(data)), "inventory values are outside the proof pool and must charge their owned string backing")
}
