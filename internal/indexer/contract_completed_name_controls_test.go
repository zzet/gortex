package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

type completedNameRows struct {
	graph.Reader
	rows       []*graph.Node
	counts     map[string]int
	calls      [][]string
	finish     error
	cancel     context.CancelFunc
	unexpected bool
}

func (r *completedNameRows) VisitNodesByNameContext(ctx context.Context, name string, yield func(*graph.Node) bool) error {
	return r.VisitNodesByNamesContext(ctx, []string{name}, yield)
}
func (r *completedNameRows) VisitNodesByNamesContext(ctx context.Context, names []string, yield func(*graph.Node) bool) error {
	r.calls = append(r.calls, append([]string(nil), names...))
	for _, name := range names {
		for _, n := range r.rows {
			if n.Name == name {
				if !yield(n) {
					return ctx.Err()
				}
			}
		}
		for i := 0; i < r.counts[name]; i++ {
			if !yield(&graph.Node{ID: "a/ignored", Name: name, Kind: graph.KindContract, RepoPrefix: "a"}) {
				return ctx.Err()
			}
		}
	}
	if r.unexpected {
		if !yield(nil) {
			return ctx.Err()
		}
		if !yield(&graph.Node{ID: "a/outside::Other", Name: "Other", Kind: graph.KindContract, RepoPrefix: "a"}) {
			return ctx.Err()
		}
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.finish != nil {
		return r.finish
	}
	return ctx.Err()
}
func completedNameEvidence(t *testing.T, r graph.Reader) *contractFollowupEvidence {
	t.Helper()
	s := newFTSStore(t)
	return &contractFollowupEvidence{Store: s, scratch: s, ctx: t.Context(), core: r, allowedRepos: map[string]bool{"a": true}}
}

func TestContractCompletedNamesKeepDuplicateEmptyAndRawSpelling(t *testing.T) {
	raw := "bad\xff"
	replacement := "bad\ufffd"
	r := &completedNameRows{Reader: graph.New(), rows: []*graph.Node{{ID: "a/f::Empty", Name: "", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f"}, {ID: "a/f::Replacement", Name: replacement, Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f"}}}
	e := completedNameEvidence(t, r)
	first := e.FindNodesByNames([]string{"", raw, replacement})
	require.NoError(t, e.err)
	require.Len(t, first[""], 1)
	require.Empty(t, first[raw])
	require.Len(t, first[replacement], 1)
	require.Equal(t, first, e.FindNodesByNames([]string{"", raw, replacement}))
	require.Len(t, r.calls, 1, "exact raw negative and replacement positive are distinct completion markers")
	e.FindNodesByNames([]string{replacement, replacement})
	require.NoError(t, e.err)
	require.Len(t, r.calls, 2, "duplicate batch retains legacy dispatch, even for completed names")
	require.Equal(t, []string{replacement, replacement}, r.calls[1])
	require.Equal(t, 1, e.completedCoreNames[replacement], "a duplicate visit must not overwrite original raw count")
}

func TestContractCompletedNamesRejectIncompleteVisits(t *testing.T) {
	for _, mode := range []string{"read_error", "canceled", "limit", "scratch_error", "scratch_final_negative", "ownership", "unexpected"} {
		t.Run(mode, func(t *testing.T) {
			r := &completedNameRows{Reader: graph.New(), rows: []*graph.Node{{ID: "a/f::Positive", Name: "Positive", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f"}}}
			e := completedNameEvidence(t, r)
			cause := errors.New("selected read failed after a row")
			switch mode {
			case "read_error":
				r.finish = cause
			case "canceled":
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				e.ctx = ctx
				r.cancel = cancel
			case "limit":
				r.counts = map[string]int{"Positive": graph.ContractProjectionRowLimit}
			case "scratch_error":
				require.NoError(t, e.scratch.Close())
			case "scratch_final_negative":
				r.rows = nil
				require.NoError(t, e.scratch.Close())
			case "ownership":
				r.rows[0].RepoPrefix = "outside"
			case "unexpected":
				r.unexpected = true
			}
			got := e.FindNodesByNames([]string{"Positive", "Negative"})
			if mode == "unexpected" {
				require.NoError(t, e.err)
				require.NotNil(t, got)
			} else {
				require.Error(t, e.err)
				require.Nil(t, got)
			}
			require.Empty(t, e.completedCoreNames, "partial/uncertifiable hydration installs no markers")
			if mode == "read_error" {
				require.ErrorIs(t, e.err, cause)
			}
			if mode == "canceled" {
				require.ErrorIs(t, e.err, context.Canceled)
			}
			if mode == "limit" {
				require.ErrorIs(t, e.err, graph.ErrContractProjectionLimit)
			}
		})
	}
}

func TestContractCompletedNamesRetainCombinedRawRowBudget(t *testing.T) {
	t.Run("uneven_batch_counts", func(t *testing.T) {
		r := &completedNameRows{Reader: graph.New(), counts: map[string]int{"Small": 2, "Large": 5}}
		e := completedNameEvidence(t, r)
		require.Empty(t, e.FindNodesByNames([]string{"Small", "Large"}))
		require.NoError(t, e.err)
		require.Equal(t, map[string]int{"Small": 2, "Large": 5}, e.completedCoreNames, "batched raw counts belong to each exact name")
		require.Empty(t, e.FindNodesByNames([]string{"Small", "Large"}))
		require.NoError(t, e.err)
		require.Len(t, r.calls, 1)
	})
	for _, mode := range []string{"both_cached", "cached_and_new"} {
		t.Run(mode, func(t *testing.T) {
			r := &completedNameRows{Reader: graph.New(), counts: map[string]int{"Large": 40000, "Other": 30000}}
			e := completedNameEvidence(t, r)
			require.Empty(t, e.FindNodesByName("Large"))
			require.NoError(t, e.err)
			require.Equal(t, 40000, e.completedCoreNames["Large"], "ignored contract rows still consume core budget")
			if mode == "both_cached" {
				require.Empty(t, e.FindNodesByName("Other"))
				require.NoError(t, e.err)
			}
			calls := len(r.calls)
			require.Nil(t, e.FindNodesByNames([]string{"Large", "Other"}))
			require.ErrorIs(t, e.err, graph.ErrContractProjectionLimit)
			if mode == "both_cached" {
				require.Len(t, r.calls, calls, "cached raw counts alone enforce cap")
			} else {
				require.NotContains(t, e.completedCoreNames, "Other", "stopped fresh suffix must not be certified")
			}
		})
	}
}

func TestContractCompletedNamesBoundMarkersWithoutRefusingLookup(t *testing.T) {
	for _, mode := range []string{"entries", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			r := &completedNameRows{Reader: graph.New()}
			e := completedNameEvidence(t, r)
			e.completedCoreNames = map[string]int{}
			if mode == "entries" {
				for i := 0; i < graph.ContractProjectionRowLimit; i++ {
					name := fmt.Sprintf("entry%d", i)
					e.completedCoreNames[name] = 0
					e.completedCoreNameBytes += len(name)
				}
			} else {
				name := strings.Repeat("x", contractCoreReceiptPayloadLimit)
				e.completedCoreNames[name] = 0
				e.completedCoreNameBytes = len(name)
			}
			count := len(e.completedCoreNames)
			bytes := e.completedCoreNameBytes
			require.Empty(t, e.FindNodesByName("Uncached"))
			require.NoError(t, e.err)
			require.Empty(t, e.FindNodesByName("Uncached"))
			require.NoError(t, e.err)
			require.Len(t, r.calls, 2, "full marker budget falls back to complete checked core visits")
			require.Len(t, e.completedCoreNames, count)
			require.Equal(t, bytes, e.completedCoreNameBytes)
		})
	}
}

func TestContractCompletedNamesNeverReplaceFinalAcceptedSourceFence(t *testing.T) {
	options, _, idx, _ := currentReceiptBaselineFixture(t, nil)
	require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), options, "fixture"))
	inputs, err := options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	snap, _, err := NewContractFollowupCapture(options)(t.Context(), nil, inputs, "fixture", "")
	require.NoError(t, err)
	defer snap.Release()
	e := completedNameEvidence(t, snap.Core)
	e.allowedRepos = map[string]bool{"fixture": true}
	first := e.FindNodesByName("register")
	require.NoError(t, e.err)
	require.NotEmpty(t, first)
	require.NoError(t, snap.ValidateAccepted(t.Context()))
	require.Len(t, snap.Files, 1)
	_, err = snap.ReadAccepted(t.Context(), snap.Files[0])
	require.NoError(t, err)
	// Raw bytes are checked by ReadAccepted; the final census/source fence
	// checks the accepted inventory and source authority, not unindexed writes.
	file := filepath.Join(idx.RootPath(), "routes.go")
	writeFile(t, file, "package fixture\nfunc register() {}\n// different physical source\n")
	_, err = snap.ReadAccepted(t.Context(), snap.Files[0])
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	mutation, err := options.Materializer.Leases.AcquireBaseCorpusMutation(t.Context(), "fixture", idx.RootPath())
	require.NoError(t, err)
	defer mutation.Release()
	require.NoError(t, mutation.Complete("newer-accepted-source"))
	mutation.Release()
	require.Equal(t, first, e.FindNodesByName("register"))
	require.NoError(t, e.err)
	require.ErrorIs(t, snap.ValidateAccepted(t.Context()), graphview.ErrBaseCorpusChanged)
}

func TestContractCompletedNamesCancelBeforeCachedLookup(t *testing.T) {
	r := &completedNameRows{Reader: graph.New(), rows: []*graph.Node{{ID: "a/f::Positive", Name: "Positive", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/f"}}}
	e := completedNameEvidence(t, r)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.ctx = ctx
	require.Len(t, e.FindNodesByName("Positive"), 1)
	require.NoError(t, e.err)
	require.Equal(t, 1, e.completedCoreNames["Positive"])
	cancel()
	require.Nil(t, e.FindNodesByName("Positive"))
	require.ErrorIs(t, e.err, context.Canceled)
	require.Len(t, r.calls, 1, "cached hydration must not conceal request cancellation")
}
