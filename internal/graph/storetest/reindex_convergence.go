package storetest

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// RunReindexConvergencePayload pins the payload and receipt rule shared by stores.
func RunReindexConvergencePayload(t *testing.T, factory Factory) {
	for _, shape := range []string{"single_stationary", "retarget_stationary", "retarget_refreshed", "refresh_stationary", "two_movers", "refresh_then_retarget_wins", "refresh_then_retarget_loses"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", shape, reverse), func(t *testing.T) {
				s := factory(t)
				s.AddNode(&graph.Node{ID: "caller", FilePath: "a.go", RepoPrefix: "repo", Kind: graph.KindFunction})
				destination := graph.Edge{From: "caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
					FilePath: "a.go", Line: 10, Origin: "stationary", Confidence: 0.9,
					Meta: map[string]any{"payload": "stationary"}}
				moving := destination
				moving.To, moving.Origin, moving.Confidence = "old::First", "mover", 0.2
				moving.Meta = map[string]any{"payload": "mover"}
				if shape == "refresh_stationary" {
					moving.To, moving.Line = destination.To, 20
				}
				refreshThenRetarget := shape == "refresh_then_retarget_wins" || shape == "refresh_then_retarget_loses"
				if refreshThenRetarget {
					moving.To = "unresolved::Original"
				}
				candidate := moving
				candidate.To, candidate.Line = destination.To, destination.Line
				batch := []graph.EdgeReindex{{Edge: &candidate, OldTo: moving.To}}
				want := destination
				wantWork := false
				s.AddEdge(&moving)
				if shape == "two_movers" {
					second := moving
					second.To, second.Origin = "old::Second", "second"
					second.Meta = map[string]any{"payload": "second"}
					s.AddEdge(&second)
					next := second
					next.To = destination.To
					batch = append(batch, graph.EdgeReindex{Edge: &next, OldTo: second.To})
					want, wantWork = candidate, true
					if reverse {
						want = next
					}
				} else if shape != "refresh_then_retarget_wins" {
					s.AddEdge(&destination)
				}
				if refreshThenRetarget {
					refreshed := moving
					refreshed.Origin, refreshed.Confidence = "refreshed", 0.8
					refreshed.Meta = map[string]any{"payload": "refreshed"}
					batch = []graph.EdgeReindex{
						{Edge: &refreshed, OldTo: moving.To, RefreshIdentity: true,
							OldFilePath: moving.FilePath, OldLine: moving.Line},
						batch[0],
					}
					if shape == "refresh_then_retarget_wins" {
						want, wantWork = candidate, true
					}
				}
				if shape == "retarget_refreshed" {
					refreshed := destination
					refreshed.Origin = "refreshed"
					refreshed.Meta = map[string]any{"payload": "refreshed"}
					batch = append(batch, graph.EdgeReindex{Edge: &refreshed, OldTo: destination.To,
						RefreshIdentity: true, OldFilePath: destination.FilePath, OldLine: destination.Line})
					want, wantWork = refreshed, true
				}
				if shape == "refresh_stationary" {
					batch[0].RefreshIdentity = true
					batch[0].OldFilePath, batch[0].OldLine = moving.FilePath, moving.Line
				}
				if reverse && len(batch) == 2 {
					batch[0], batch[1] = batch[1], batch[0]
				}
				receipts := s.(graph.MutationReceiptStore)
				token := receipts.BeginMutationReceipt()
				if shape == "single_stationary" {
					s.ReindexEdge(batch[0].Edge, batch[0].OldTo)
				} else {
					s.ReindexEdges(batch)
				}
				receipt := receipts.EndMutationReceipt(token)
				for name, rows := range map[string][]*graph.Edge{
					"all": s.AllEdges(), "out": s.GetOutEdges("caller"), "in": s.GetInEdges(destination.To),
				} {
					if len(rows) != 1 {
						t.Errorf("%s row count = %d, want 1", name, len(rows))
						continue
					}
					if !reflect.DeepEqual(*rows[0], want) {
						t.Errorf("%s payload = %+v, want %+v", name, *rows[0], want)
					}
				}
				if got := s.GetInEdges("old::First"); len(got) != 0 {
					t.Errorf("old incoming rows = %+v", got)
				}
				if got := s.GetInEdges("unresolved::Original"); len(got) != 0 {
					t.Errorf("refreshed old incoming rows = %+v", got)
				}
				if got := s.GetInEdges("old::Second"); len(got) != 0 {
					t.Errorf("second old incoming rows = %+v", got)
				}
				if err := s.VerifyEdgeIdentities(); err != nil {
					t.Fatal(err)
				}
				var wantFiles []string
				if wantWork {
					wantFiles = []string{"a.go"}
				}
				if !reflect.DeepEqual(receipt.UnresolvedFiles, wantFiles) || !reflect.DeepEqual(receipt.ChangedFiles, wantFiles) {
					t.Errorf("receipt = %+v, want winner files %v", receipt, wantFiles)
				}
				if refreshThenRetarget {
					var wantTargets []string
					if wantWork {
						wantTargets = []string{destination.To}
					}
					if !reflect.DeepEqual(receipt.TargetIDs, wantTargets) {
						t.Errorf("receipt targets = %v, want %v", receipt.TargetIDs, wantTargets)
					}
				}
			})
		}
	}
}

// RunReindexMovingSiblings checks that a batch frees its old keys together.
func RunReindexMovingSiblings(t *testing.T, factory Factory) {
	for _, final := range []string{"X", "Z"} {
		t.Run("intermediate_to_"+final, func(t *testing.T) {
			s := factory(t)
			original := graph.Edge{From: "caller", To: "X", Kind: graph.EdgeCalls,
				FilePath: "a.go", Line: 10, Origin: "original", Confidence: 0.2}
			s.AddEdge(&original)
			intermediate := original
			intermediate.To, intermediate.Origin = "Y", "intermediate"
			last := original
			last.To, last.Origin, last.Confidence = final, "last", 0.9
			s.ReindexEdges([]graph.EdgeReindex{
				{Edge: &intermediate, OldTo: "X"},
				{Edge: &last, OldTo: "Y"},
			})
			for name, rows := range map[string][]*graph.Edge{
				"all": s.AllEdges(), "out": s.GetOutEdges("caller"), "in": s.GetInEdges(final),
			} {
				if len(rows) != 1 || !reflect.DeepEqual(*rows[0], last) {
					t.Errorf("%s rows = %+v, want only %+v", name, rows, last)
				}
			}
			for _, target := range []string{"X", "Y", "Z"} {
				if target != final && len(s.GetInEdges(target)) != 0 {
					t.Errorf("dangling incoming rows at %s: %+v", target, s.GetInEdges(target))
				}
			}
			if s.EdgeCount() != 1 {
				t.Errorf("edge count = %d, want 1", s.EdgeCount())
			}
			if err := s.VerifyEdgeIdentities(); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("intermediate_converges_after_first_mover", func(t *testing.T) {
		s := factory(t)
		original := graph.Edge{From: "caller", To: "X", Kind: graph.EdgeCalls,
			FilePath: "a.go", Line: 10, Origin: "original"}
		first := original
		first.To, first.Origin = "W", "first"
		s.AddEdge(&original)
		s.AddEdge(&first)
		intermediate, winner, last := original, first, original
		intermediate.To, winner.To, last.To = "Y", "Z", "Z"
		last.Origin = "last"
		s.ReindexEdges([]graph.EdgeReindex{
			{Edge: &intermediate, OldTo: "X"},
			{Edge: &winner, OldTo: "W"},
			{Edge: &last, OldTo: "Y"},
		})
		for name, rows := range map[string][]*graph.Edge{
			"all": s.AllEdges(), "out": s.GetOutEdges("caller"), "in": s.GetInEdges("Z"),
		} {
			if len(rows) != 1 || !reflect.DeepEqual(*rows[0], winner) {
				t.Errorf("%s rows = %+v, want only %+v", name, rows, winner)
			}
		}
		for _, target := range []string{"X", "W", "Y"} {
			if len(s.GetInEdges(target)) != 0 {
				t.Errorf("dangling incoming rows at %s", target)
			}
		}
		if err := s.VerifyEdgeIdentities(); err != nil {
			t.Fatal(err)
		}
	})
	for _, shape := range []string{"target_chain", "target_swap", "line_chain"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%t", shape, reverse), func(t *testing.T) {
				s := factory(t)
				first := graph.Edge{From: "caller", To: "X", Kind: graph.EdgeCalls,
					FilePath: "a.go", Line: 10, Origin: "first", Confidence: 0.2}
				second := first
				second.To, second.Origin, second.Confidence = "Y", "second", 0.9
				if shape == "line_chain" {
					second.To, second.Line = first.To, 11
				}
				s.AddEdge(&first)
				s.AddEdge(&second)
				wantFirst, wantSecond := first, second
				wantFirst.To, wantSecond.To = "Y", "Z"
				if shape == "target_swap" {
					wantSecond.To = "X"
				}
				batch := []graph.EdgeReindex{{Edge: &wantFirst, OldTo: first.To}, {Edge: &wantSecond, OldTo: second.To}}
				if shape == "line_chain" {
					wantFirst.To, wantSecond.To = first.To, second.To
					wantFirst.Line, wantSecond.Line = 11, 12
					for i, old := range []*graph.Edge{&first, &second} {
						batch[i].RefreshIdentity = true
						batch[i].OldFilePath, batch[i].OldLine = old.FilePath, old.Line
					}
				}
				if reverse {
					batch[0], batch[1] = batch[1], batch[0]
				}
				s.ReindexEdges(batch)
				out := s.GetOutEdges("caller")
				if len(out) != 2 || s.EdgeCount() != 2 || len(s.AllEdges()) != 2 {
					t.Fatalf("surviving edges = %d, count = %d, all = %d, want 2", len(out), s.EdgeCount(), len(s.AllEdges()))
				}
				for _, want := range []graph.Edge{wantFirst, wantSecond} {
					foundOut, foundIn := false, false
					for _, row := range out {
						foundOut = foundOut || reflect.DeepEqual(*row, want)
					}
					for _, row := range s.GetInEdges(want.To) {
						foundIn = foundIn || reflect.DeepEqual(*row, want)
					}
					if !foundOut || !foundIn {
						t.Errorf("payload %+v: outgoing=%t incoming=%t", want, foundOut, foundIn)
					}
				}
				if err := s.VerifyEdgeIdentities(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
