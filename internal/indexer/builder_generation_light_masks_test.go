package indexer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

func TestWriteMasksPreservesMetadataHeavyPayloadAndOrphanClaims(t *testing.T) {
	store := builderOpenStore(t, "light-mask-parity")
	builder := &SparseGenerationBuilder{Store: store, Logger: zap.NewNop()}
	covered := builderGraphPath(builderRepoPrefix, "covered.go")
	extra := builderGraphPath(builderRepoPrefix, "payload-only.go")
	contextPath := builderGraphPath(builderRepoPrefix, "context.go")
	deleted := builderGraphPath(builderRepoPrefix, "deleted.go")
	ownerID, orphanID, keptID := covered+"::Owner", "builtin::orphan", "builtin::kept"
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: ownerID, Kind: graph.KindFunction, FilePath: covered},
		{ID: orphanID, Kind: graph.KindFunction},
		{ID: keptID, Kind: graph.KindFunction},
	}, []*graph.Edge{
		{From: ownerID, To: orphanID, Kind: graph.EdgeCalls, FilePath: covered},
		{From: ownerID, To: keptID, Kind: graph.EdgeCalls, FilePath: covered},
	})
	plan := buildPlan{
		indexed: []string{"covered.go", "context.go", "missing.go"},
		deleted: []string{"deleted.go"}, withdrawn: map[string]struct{}{contextPath: {}},
	}
	type claims struct {
		files      []store_sqlite.FileMask
		tombstones []string
		sources    []store_sqlite.EdgeSourceMask
		report     BuildReport
	}
	var baseline claims
	for _, heavy := range []bool{false, true} {
		handle := builderContextDerivedHandle(t, store)
		nodes := []*graph.Node{
			{ID: ownerID, Kind: graph.KindFunction, FilePath: covered, RepoPrefix: builderRepoPrefix},
			{ID: extra + "::Extra", Kind: graph.KindFunction, FilePath: extra, RepoPrefix: builderRepoPrefix},
			{ID: keptID, Kind: graph.KindFunction},
		}
		if heavy {
			for _, node := range nodes {
				node.Meta = map[string]interface{}{
					"doc":       strings.Repeat("documentation ", 8192),
					"signature": strings.Repeat("signature ", 8192),
					"opaque":    map[string]interface{}{"payload": strings.Repeat("metadata ", 8192)},
				}
			}
		}
		handle.AddBatch(nodes, []*graph.Edge{{From: keptID, To: ownerID, Kind: graph.EdgeCalls, FilePath: covered}})
		var got claims
		if err := builder.writeMasks(BuildRequest{RepoPrefix: builderRepoPrefix, Base: base}, plan, handle, &got.report); err != nil {
			t.Fatal(err)
		}
		var err error
		if got.files, err = handle.FileMasks(); err != nil {
			t.Fatal(err)
		}
		if got.tombstones, err = handle.NodeTombstones(); err != nil {
			t.Fatal(err)
		}
		if got.sources, err = handle.EdgeSourceMasks(); err != nil {
			t.Fatal(err)
		}
		wantFiles := []store_sqlite.FileMask{
			{RepoPrefix: builderRepoPrefix, FilePath: contextPath, Mode: store_sqlite.OwnershipContext},
			{RepoPrefix: builderRepoPrefix, FilePath: covered, Mode: store_sqlite.OwnershipReplace},
			{RepoPrefix: builderRepoPrefix, FilePath: deleted, Mode: store_sqlite.OwnershipDelete},
			{RepoPrefix: builderRepoPrefix, FilePath: extra, Mode: store_sqlite.OwnershipReplace},
		}
		if !reflect.DeepEqual(got.files, wantFiles) || !reflect.DeepEqual(got.tombstones, []string{keptID, orphanID}) {
			t.Fatalf("heavy=%t: file claims=%+v tombstones=%v", heavy, got.files, got.tombstones)
		}
		if got.report.OrphanStubTombstones != 1 || got.report.UnmaskedPayloadNodes != 1 ||
			!reflect.DeepEqual(got.report.PlannedNotCovered, []string{"missing.go"}) {
			t.Fatalf("heavy=%t: orphan/payload/planned claims=%+v", heavy, got.report)
		}
		if !heavy {
			baseline = got
		} else if !reflect.DeepEqual(got, baseline) {
			t.Fatalf("metadata changed mask or orphan semantics: got %+v, baseline %+v", got, baseline)
		}
	}
}
