package graphview_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

var incomingPublicBenchmarkPage graph.BoundedIncomingSourceProjection
var incomingPublicBenchmarkError error

type incomingPublicBenchmarkFixture struct {
	view           *graph.OverlaidView
	lower, upper   *store_sqlite.Store
	target         string
	want           []string
	sites, markers int
}

func prepareIncomingPublicBenchmark(b *testing.B, sites int, distinct bool, markers int) incomingPublicBenchmarkFixture {
	b.Helper()
	control, err := store_sqlite.Open(filepath.Join(b.TempDir(), "incoming-public.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = control.Close() })
	allocate := func(base int64) int64 {
		id, err := control.Catalog().CreateViewGeneration(b.Context(), store_sqlite.ViewGeneration{
			OwnerKind: "dedicated_graph", GraphID: "incoming-public-benchmark", GenerationKind: "dedicated", BaseGenerationID: base,
			TreeOID: "benchmark-source", ConfigHash: "benchmark-policy", State: store_sqlite.ViewGenerationBuilding, CreatedAt: 1,
		})
		if err != nil {
			b.Fatal(err)
		}
		return id
	}
	lowerID := allocate(0)
	lower := control.AtGeneration(lowerID)
	target := "alpha/target.cs::Target"
	nodes := []*graph.Node{{ID: target, Name: "Target", RepoPrefix: "alpha", FilePath: "alpha/target.cs", Kind: graph.KindType}}
	edges := make([]*graph.Edge, sites)
	want := make([]string, 0)
	for i := 0; i < sites; i++ {
		sourceIndex := 0
		if distinct {
			sourceIndex = i
		}
		id := fmt.Sprintf("alpha/source.cs::S%05d", sourceIndex)
		if distinct || i == 0 {
			nodes = append(nodes, &graph.Node{ID: id, Name: fmt.Sprintf("S%05d", sourceIndex), RepoPrefix: "alpha", FilePath: "alpha/source.cs", Kind: graph.NodeKind("function")})
			want = append(want, id)
		}
		edges[i] = &graph.Edge{From: id, To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/calls.cs", Line: i + 1}
	}
	lower.AddBatch(nodes, edges)
	if err := control.Catalog().PublishViewGeneration(b.Context(), lowerID, 2); err != nil {
		b.Fatal(err)
	}
	upperID := allocate(lowerID)
	upper := control.AtGeneration(upperID)
	markerIDs := make([]string, markers)
	markerNodes := make([]*graph.Node, markers)
	for i := range markerIDs {
		markerIDs[i] = fmt.Sprintf("alpha/unrelated.cs::M%05d", i)
		markerNodes[i] = &graph.Node{ID: markerIDs[i], Name: "Unrelated", RepoPrefix: "alpha", FilePath: "alpha/unrelated.cs", Kind: graph.KindType}
	}
	upper.AddBatch(markerNodes, nil)
	if err := upper.SetNodeIdentityReplacements(markerIDs); err != nil {
		b.Fatal(err)
	}
	if err := control.PublishPayloadGeneration(b.Context(), upperID, 3); err != nil {
		b.Fatal(err)
	}
	control.AddBatch([]*graph.Node{{ID: "gen0-poison", Name: "Poison", RepoPrefix: "alpha", FilePath: "alpha/poison.cs", Kind: graph.KindType}}, []*graph.Edge{{From: "gen0-poison", To: target, Kind: graph.EdgeKind("calls"), FilePath: "alpha/poison.cs", Line: 1}})
	if lower.EdgeCount() != sites || upper.EdgeCount() != 0 || upper.NodeCount() != markers {
		b.Fatal("benchmark payload/mask fixture mismatch")
	}
	gen0, err := control.FindIncomingSourcesBounded(b.Context(), []string{target}, graph.EdgeKind("calls"), 1)
	if err != nil || gen0.Truncated[target] || !reflect.DeepEqual(gen0.Sources[target], []string{"gen0-poison"}) {
		b.Fatalf("gen0 poison prerequisite: %+v %v", gen0, err)
	}
	layer, err := graphview.NewGenerationLayer(upper)
	if err != nil {
		b.Fatal(err)
	}
	return incomingPublicBenchmarkFixture{view: graph.NewOverlaidViewWithLayer(lower, layer), lower: lower, upper: upper, target: target, want: want, sites: sites, markers: markers}
}

func checkIncomingPublicBenchmarkResult(b *testing.B, fixture incomingPublicBenchmarkFixture, limit int, page graph.BoundedIncomingSourceProjection, err error) bool {
	b.Helper()
	if err != nil {
		var bounded *graph.BoundedLocalizationLimitError
		// Historical global marker refusal is measured explicitly on BEFORE,
		// never skipped or reported as successful equivalent work. These inputs
		// fit the new raw-work bound exactly, so AFTER work refusal is an error.
		if fixture.markers != 1024 || !errors.As(err, &bounded) || bounded.Resource != "overlay incoming-source detached shadows" || bounded.Limit != 256 || len(page.Sources) != 0 || len(page.Truncated) != 0 {
			b.Fatalf("unexpected public benchmark refusal: %+v %v", page, err)
		}
		return true
	}
	wantTruncated := len(fixture.want) > limit
	if page.Truncated[fixture.target] != wantTruncated {
		b.Fatalf("source truncation mismatch: %+v", page)
	}
	if wantTruncated {
		if len(page.Sources[fixture.target]) != 0 {
			b.Fatal("truncated result retained partial sources")
		}
	} else if len(fixture.want) == 0 {
		if len(page.Sources[fixture.target]) != 0 {
			b.Fatal("empty fixture gained a source")
		}
	} else if !reflect.DeepEqual(page.Sources[fixture.target], fixture.want) {
		b.Fatalf("public benchmark wrong sources: %+v want %v", page, fixture.want)
	}
	for target := range page.Sources {
		if target != fixture.target {
			b.Fatal("unrequested source target")
		}
	}
	for target := range page.Truncated {
		if target != fixture.target {
			b.Fatal("unrequested truncated target")
		}
	}
	return false
}

// Existing public API only: this exact authored file can compile on BEFORE and
// AFTER. Fixture setup, layer construction, first lazy load, and semantic checks
// are outside timing. This measures warm-layer public query + outcome counters,
// not construction, parsing, physical disk I/O alone, or end-to-end indexing.
func BenchmarkIncomingSourcePublicBeforeAfter(b *testing.B) {
	for _, sites := range []int{0, 300, 16_384} {
		for _, distinct := range []bool{false, true} {
			for _, limit := range []int{1, 10} {
				for _, markers := range []int{0, 1024} {
					b.Run(fmt.Sprintf("sites_%d_distinct_%t_limit_%d_markers_%d", sites, distinct, limit, markers), func(b *testing.B) {
						fixture := prepareIncomingPublicBenchmark(b, sites, distinct, markers)
						ids := []string{fixture.target}
						ctx := b.Context()
						page, err := fixture.view.FindIncomingSourcesBounded(ctx, ids, graph.EdgeKind("calls"), limit)
						preflightRefused := checkIncomingPublicBenchmarkResult(b, fixture, limit, page, err)
						var successful, refused int
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							page, err = fixture.view.FindIncomingSourcesBounded(ctx, ids, graph.EdgeKind("calls"), limit)
							if err != nil {
								refused++
							} else {
								successful++
							}
						}
						b.StopTimer()
						incomingPublicBenchmarkPage, incomingPublicBenchmarkError = page, err
						finalRefused := checkIncomingPublicBenchmarkResult(b, fixture, limit, page, err)
						if finalRefused != preflightRefused || successful+refused != b.N || preflightRefused && refused != b.N || !preflightRefused && successful != b.N {
							b.Fatal("immutable benchmark outcome changed during timing")
						}
						if fixture.lower.EdgeCount() != sites || fixture.upper.EdgeCount() != 0 || fixture.upper.NodeCount() != markers {
							b.Fatal("public query mutated fixture payload")
						}
						b.ReportMetric(float64(successful)/float64(b.N), "successful/op")
						b.ReportMetric(float64(refused)/float64(b.N), "refused/op")
						b.ReportMetric(float64(sites), "input-sites")
						b.ReportMetric(float64(markers), "unrelated-markers")
					})
				}
			}
		}
	}
}
