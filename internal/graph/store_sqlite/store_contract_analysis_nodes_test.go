package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestContractAnalysisNodesContextExactMetadataAndBounds(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "analysis.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	nodes := []*graph.Node{{ID: "a", Kind: graph.KindConfigKey, RepoPrefix: "repo", Meta: map[string]any{"key": "clock"}}, {ID: "b", Kind: graph.KindMethod, RepoPrefix: "repo", Meta: map[string]any{"via": "spring.Bean"}}}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	other := s.AtGeneration(7)
	if err := other.AddBatchChecked([]*graph.Node{{ID: "a", Kind: graph.KindConfigKey, Meta: map[string]any{"key": "other"}}}, nil); err != nil {
		t.Fatal(err)
	}
	got, truncated, err := s.ContractAnalysisNodesContext(context.Background(), 2)
	if err != nil || truncated || len(got) != 2 || got[0].Meta["key"] != "clock" || got[1].Meta["via"] != "spring.Bean" {
		t.Fatalf("full evidence=%#v truncated=%v err=%v", got, truncated, err)
	}
	got, truncated, err = other.ContractAnalysisNodesContext(context.Background(), 2)
	if err != nil || truncated || len(got) != 1 || got[0].Meta["key"] != "other" {
		t.Fatalf("generation evidence=%#v %v %v", got, truncated, err)
	}
	got, truncated, err = s.ContractAnalysisNodesContext(context.Background(), 1)
	if err != nil || !truncated || len(got) != 1 {
		t.Fatalf("bound=%#v %v %v", got, truncated, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, _, err = s.ContractAnalysisNodesContext(ctx, 2)
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("cancel=%#v %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	got, _, err = s.ContractAnalysisNodesContext(context.Background(), 2)
	if err == nil || got != nil {
		t.Fatalf("closed=%#v %v", got, err)
	}
}
