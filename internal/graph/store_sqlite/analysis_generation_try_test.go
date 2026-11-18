package store_sqlite

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.TryAnalysisQueryStore = (*Store)(nil)

func TestTryAnalysisMetricsNeverWaitForWriter(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generationID := buildMinimalAnalysisGeneration(t, store, "try", 0, true)

	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		store.writeMu.Lock()
		defer store.writeMu.Unlock()
		close(locked)
		<-release
	}()
	<-locked
	defer func() { close(release); <-done }()

	result := make(chan error, 1)
	go func() {
		_, err := store.TryAnalysisNodeMetrics(generationID, []string{"try-node"})
		if !errors.Is(err, graph.ErrAnalysisStoreBusy) {
			result <- err
			return
		}
		_, _, err = store.TryTopAnalysisNodeMetrics(generationID, graph.AnalysisMetricAuthority, 1, nil)
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, graph.ErrAnalysisStoreBusy) {
			t.Fatalf("try metrics error = %v, want busy", err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("try metrics waited for writer")
	}
}

func TestTryAnalysisMetricsMatchBlockingReads(t *testing.T) {
	store, err := Open(filepathForAnalysisTest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generationID := buildMinimalAnalysisGeneration(t, store, "try", 0, true)
	for _, ids := range [][]string{nil, {"try-node", "missing", "try-node"}, {""}} {
		want, wantErr := store.AnalysisNodeMetrics(generationID, ids)
		got, gotErr := store.TryAnalysisNodeMetrics(generationID, ids)
		if !reflect.DeepEqual(want, got) || !sameAnalysisError(wantErr, gotErr) {
			t.Fatalf("ids %v: try=%+v, %v; blocking=%+v, %v", ids, got, gotErr, want, wantErr)
		}
	}
	for _, metric := range []graph.AnalysisMetric{graph.AnalysisMetricPageRank, graph.AnalysisMetricAuthority, graph.AnalysisMetricHub, "invalid"} {
		for _, cursor := range []*graph.AnalysisMetricCursor{nil, {Score: 1, RowID: 1}, {RowID: -1}} {
			want, wantNext, wantErr := store.TopAnalysisNodeMetrics(generationID, metric, 1, cursor)
			got, gotNext, gotErr := store.TryTopAnalysisNodeMetrics(generationID, metric, 1, cursor)
			if !reflect.DeepEqual(want, got) || !reflect.DeepEqual(wantNext, gotNext) || !sameAnalysisError(wantErr, gotErr) {
				t.Fatalf("metric %s, cursor %+v: try=%+v, %+v, %v; blocking=%+v, %+v, %v", metric, cursor, got, gotNext, gotErr, want, wantNext, wantErr)
			}
		}
	}
	buildMinimalAnalysisGeneration(t, store, "next", 0, true)
	if _, err := store.TryAnalysisNodeMetrics(generationID, []string{"try-node"}); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("inactive point query: %v", err)
	}
	if _, _, err := store.TryTopAnalysisNodeMetrics(generationID, graph.AnalysisMetricHub, 1, nil); !errors.Is(err, graph.ErrAnalysisGenerationInactive) {
		t.Fatalf("inactive top query: %v", err)
	}
}

func sameAnalysisError(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Error() == b.Error()
}
