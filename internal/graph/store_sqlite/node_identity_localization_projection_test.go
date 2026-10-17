package store_sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestNodeIdentityLocalizationProjectionKeepsOnlyRequestedSummaries(t *testing.T) {
	s, h := newIdentityMaskStore(t)
	id := "alpha/a::N"
	s.AddNode(&graph.Node{ID: id, Name: "gen0 poison"})
	h.AddBatch([]*graph.Node{
		{ID: id, Name: "upper", FilePath: "alpha/a", RepoPrefix: "alpha", StartLine: 42, Meta: map[string]interface{}{"is_test": true, "heavy": strings.Repeat("x", 64<<10)}},
		{ID: "alpha/a::Sibling", Name: "unrequested", FilePath: "alpha/a", RepoPrefix: "alpha"},
	}, nil)
	for _, includeTest := range []bool{false, true} {
		rows, err := h.NodeIdentityLocalizationSummariesContext(t.Context(), []string{id, id}, includeTest)
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows=%v err=%v", rows, err)
		}
		if rows[0].Name != "upper" || rows[0].StartLine != 42 {
			t.Fatalf("wrong physical generation/location: %+v", rows[0])
		}
		if includeTest {
			if !reflect.DeepEqual(rows[0].Meta, map[string]interface{}{"is_test": true}) {
				t.Fatalf("unexpected metadata projection=%v", rows[0].Meta)
			}
		} else if rows[0].Meta != nil {
			t.Fatal("ordinary identity projection fetched metadata")
		}
	}
	if s.GetNode(id).Name != "gen0 poison" || h.NodeCount() != 2 {
		t.Fatal("projection mutated store")
	}
}

func TestNodeIdentityLocalizationProjectionFailsWithoutPartialRows(t *testing.T) {
	s, h := newIdentityMaskStore(t)
	id := "alpha/a::N"
	h.AddNode(&graph.Node{ID: id, Name: "upper"})
	if rows, err := h.NodeIdentityLocalizationSummariesContext(t.Context(), []string{id, "missing"}, false); !errors.Is(err, ErrGenerationMaskIntegrity) || rows != nil {
		t.Fatalf("missing row partial=%v err=%v", rows, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if rows, err := h.NodeIdentityLocalizationSummariesContext(ctx, []string{id}, false); !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatalf("canceled rows=%v err=%v", rows, err)
	}
	if rows, err := s.NodeIdentityLocalizationSummariesContext(t.Context(), []string{id}, false); !errors.Is(err, ErrMasksAtBaseGeneration) || rows != nil {
		t.Fatalf("gen0 rows=%v err=%v", rows, err)
	}
	var limitErr *graph.BoundedLocalizationLimitError
	if rows, err := h.NodeIdentityLocalizationSummariesContext(t.Context(), make([]string, 257), false); !errors.As(err, &limitErr) || rows != nil {
		t.Fatalf("unbounded rows=%v err=%v", rows, err)
	}
	if rows, err := h.NodeIdentityLocalizationSummariesContext(t.Context(), nil, false); err != nil || len(rows) != 0 {
		t.Fatalf("empty rows=%v err=%v", rows, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if rows, err := h.NodeIdentityLocalizationSummariesContext(t.Context(), []string{id}, true); err == nil || rows != nil {
		t.Fatalf("closed SQL handle yielded rows=%v err=%v", rows, err)
	}
}

func TestNodeIdentityLocalizationProjectionUsesSelectedIdentitySeeks(t *testing.T) {
	_, h := newIdentityMaskStore(t)
	for _, includeTest := range []bool{false, true} {
		rows, err := h.db.Query("EXPLAIN QUERY PLAN "+nodeIdentityLocalizationQuery(includeTest, 2), h.viewGen, "alpha/a::N", "alpha/a::Absent")
		if err != nil {
			t.Fatal(err)
		}
		foundSeek := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(detail, "SCAN nodes") {
				_ = rows.Close()
				t.Fatalf("upper payload scan: %s", detail)
			}
			if strings.Contains(detail, "SEARCH nodes") {
				foundSeek = true
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !foundSeek {
			t.Fatal("missing indexed identity seek")
		}
	}
}
