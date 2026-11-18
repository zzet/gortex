package indexer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractNameImportProbesBatchMissingRows(t *testing.T) {
	escaped := namePageNode("escaped")
	escaped.Meta = map[string]any{"\x00<>\"\\": []any{strings.Repeat("\x00<>\"\\\u2028", 32), true, -1, 1.5, nil}}
	bound, bounded := contractNameImportCandidateBound(escaped, contractFollowupCompactLimit)
	require.True(t, bounded)
	encoded, err := json.Marshal(escaped)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded)+1, bound, "pending bound includes escaped JSON, containers and a separator")
	e := completedNameEvidence(t, graph.New())
	page := contractNameImportPage{evidence: e, byteLimit: contractFollowupCompactLimit}
	var calls [][]string
	probes := contractNameImportProbes{page: &page, probe: func(ids []string) (map[string]*graph.Node, error) {
		calls = append(calls, append([]string(nil), ids...))
		return e.scratch.GetNodesByIDsContext(t.Context(), ids)
	}}
	var ids []string
	for i := 0; i < 257; i++ {
		node := namePageNode(fmt.Sprint(i))
		ids = append(ids, node.ID)
		require.True(t, probes.add(node))
		require.Equal(t, ((i+1)/128)*128, e.writtenNodes, "full write pages commit before the next visitor row")
		require.LessOrEqual(t, len(probes.nodes)+len(page.nodes), contractFrontierReadBatchSize)
		require.LessOrEqual(t, probes.boundedBytes+page.encodedBytes+contractNameImportEnvelopeBytes, page.byteLimit)
	}
	require.True(t, probes.flush())
	require.True(t, page.flush())
	require.NoError(t, e.err)
	require.Len(t, calls, 3, "257 missing rows need three private scratch probes, not one per row")
	require.Len(t, calls[0], 128)
	require.Len(t, calls[1], 128)
	require.Len(t, calls[2], 1)
	got, err := e.scratch.GetNodesByIDsContext(t.Context(), ids)
	require.NoError(t, err)
	require.Len(t, got, 257)
	for i, id := range ids {
		require.Equal(t, namePageNode(fmt.Sprint(i)), got[id])
	}
}

func TestContractNameImportProbesKeepCrossPagePrecedence(t *testing.T) {
	r := &completedNameRows{Reader: graph.New()}
	calls := 0
	cause := errors.New("losing duplicate must not be encoded")
	duplicate := namePageNode("0")
	duplicate.Meta["doc"] = nameImportFailingJSON{calls: &calls, err: cause}
	for i := 0; i < 128; i++ {
		if i == 64 {
			r.rows = append(r.rows, duplicate)
		}
		r.rows = append(r.rows, namePageNode(fmt.Sprint(i)))
	}
	derived := namePageNode("derived")
	derived.Meta["doc"] = "scratch wins"
	coreDerived := namePageNode("derived")
	coreDerived.Meta["doc"] = "selected row must lose"
	r.rows = append(r.rows, duplicate, coreDerived, namePageNode("tail"))
	e := completedNameEvidence(t, r)
	e.AddNode(derived)
	got := e.FindNodesByNames([]string{"Match"})
	require.NoError(t, e.err)
	require.Len(t, got["Match"], 130)
	found, err := e.scratch.GetNodesByIDsContext(t.Context(), []string{duplicate.ID, derived.ID, "a/tail"})
	require.NoError(t, err)
	require.Equal(t, "0", found[duplicate.ID].Meta["doc"], "first selected ID wins across committed pages")
	require.Equal(t, "scratch wins", found[derived.ID].Meta["doc"])
	require.Equal(t, "tail", found["a/tail"].Meta["doc"])
	require.Equal(t, 132, e.completedCoreNames["Match"], "raw duplicates within and across probe pages still count")
	require.Zero(t, calls, "neither the pending nor the committed duplicate may be encoded")
	e.AddNode(namePageNode("later"))
	require.Len(t, e.FindNodesByName("Match"), 131, "completion never freezes later scratch rows")
}

type nameImportFailingJSON struct {
	calls *int
	err   error
}

func (v nameImportFailingJSON) MarshalJSON() ([]byte, error) {
	(*v.calls)++
	return nil, v.err
}

func TestContractNameImportProbesPreserveSkippedRowEncoding(t *testing.T) {
	for _, mode := range []string{"oversize", "unsupported", "deep", "cyclic", "wide"} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/present=%t", mode, present), func(t *testing.T) {
				e := completedNameEvidence(t, graph.New())
				first := namePageNode("same")
				if present {
					e.AddNode(first)
				}
				encoded, err := json.Marshal(first)
				require.NoError(t, err)
				limit := contractNameImportEnvelopeBytes + len(encoded)
				if mode == "deep" || mode == "cyclic" || mode == "wide" {
					limit = contractFollowupCompactLimit
				}
				page := contractNameImportPage{evidence: e, byteLimit: limit}
				probeCalls := 0
				probes := contractNameImportProbes{page: &page, probe: func(ids []string) (map[string]*graph.Node, error) {
					probeCalls++
					return e.scratch.GetNodesByIDsContext(t.Context(), ids)
				}}
				candidate := namePageNode("same")
				cause := errors.New("selected loser must not be encoded")
				calls := 0
				switch mode {
				case "oversize":
					candidate.Meta["doc"] = strings.Repeat("x", limit*2)
				case "unsupported":
					candidate.Meta["doc"] = nameImportFailingJSON{calls: &calls, err: cause}
				case "deep":
					var nested any = "leaf"
					for i := 0; i < 66; i++ {
						nested = map[string]any{"next": nested}
					}
					candidate.Meta["doc"] = nested
				case "cyclic":
					cycle := map[string]any{}
					cycle["self"] = cycle
					candidate.Meta["doc"] = cycle
				case "wide":
					items := make([]any, 4097)
					for i := range items {
						items[i] = true
					}
					candidate.Meta["doc"] = items
				}
				_, bounded := contractNameImportCandidateBound(candidate, limit)
				require.False(t, bounded, "unusual candidates select single-probe fallback")
				if present {
					require.True(t, probes.add(candidate))
					require.True(t, probes.flush())
					require.True(t, page.flush())
					require.NoError(t, e.err)
					require.Zero(t, calls, "scratch precedence is checked before invoking even a custom encoder")
					found, readErr := e.scratch.GetNodesByIDsContext(t.Context(), []string{first.ID})
					require.NoError(t, readErr)
					require.Equal(t, first, found[first.ID])
					require.Equal(t, 1, e.writtenNodes)
				} else if mode == "deep" || mode == "wide" {
					require.True(t, probes.add(candidate), "traversal limits must not reject a valid missing row")
					require.True(t, probes.flush())
					require.True(t, page.flush())
					require.NoError(t, e.err)
					found, readErr := e.scratch.GetNodesByIDsContext(t.Context(), []string{candidate.ID})
					require.NoError(t, readErr)
					require.Equal(t, candidate, found[candidate.ID])
					require.Equal(t, 1, e.writtenNodes)
				} else {
					require.False(t, probes.add(candidate))
					switch mode {
					case "oversize":
						require.ErrorIs(t, e.err, graph.ErrContractProjectionLimit)
					case "unsupported":
						require.ErrorIs(t, e.err, cause)
						require.Equal(t, 1, calls)
					case "cyclic":
						var unsupported *json.UnsupportedValueError
						require.ErrorAs(t, e.err, &unsupported)
					}
					require.Zero(t, e.writtenNodes)
				}
				require.Equal(t, 1, probeCalls, "fallback performs exactly one scratch probe")
			})
		}
	}
}
