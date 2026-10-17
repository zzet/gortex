package graph

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type incomingSourcesLegacyOnlyReader struct {
	Reader
	called bool
}

func (r *incomingSourcesLegacyOnlyReader) FindIncomingSourcesBounded(_ context.Context, ids []string, _ EdgeKind, _ int) (BoundedIncomingSourceProjection, error) {
	r.called = true
	out := emptyIncomingSourceProjection()
	for _, id := range ids {
		out.Sources[id] = []string{"legacy-source"}
	}
	return out, nil
}

func TestIncomingSourcePublicWrapperKeepsLegacyGuardAndDelegationOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	keys := make([]string, MaxBoundedAdjacencyKeys+1)
	for i := range keys {
		keys[i] = fmt.Sprintf("target-%04d", i)
	}
	var absent *OverlaidView
	page, err := absent.FindIncomingSourcesBounded(ctx, keys, EdgeKind("calls"), maxBoundedIncomingSourceLimit)
	if err != nil || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("nil receiver no longer wins guards: %+v %v", page, err)
	}
	view := &OverlaidView{layer: &OverlayLayer{}}
	page, err = view.FindIncomingSourcesBounded(ctx, keys, EdgeKind("calls"), 0)
	if err != nil || len(page.Sources) != 0 || len(page.Truncated) != len(keys) {
		t.Fatalf("zero limit no longer wins cancellation/key budget: %+v %v", page, err)
	}
	base := &incomingSourcesLegacyOnlyReader{}
	view = &OverlaidView{base: base}
	page, err = view.FindIncomingSourcesBounded(context.Background(), []string{"target"}, EdgeKind("calls"), 1)
	if err != nil || !base.called || len(page.Sources["target"]) != 1 || page.Sources["target"][0] != "legacy-source" {
		t.Fatalf("unfiltered legacy-only delegation failed: %+v %v", page, err)
	}
	base.called = false
	page, err = view.FindIncomingSourcesBounded(ctx, []string{"target"}, EdgeKind("calls"), 1)
	if !errors.Is(err, context.Canceled) || base.called || len(page.Sources) != 0 {
		t.Fatalf("cancellation no longer precedes delegation: %+v %v", page, err)
	}
}

// admissionRow is a payload-free stand-in for the incoming rows a caller
// admits. AdmitIncomingRowsBounded is generic over it precisely because the
// preparation leg admits EdgeIdentity values and the resolution leg admits
// *Edge pointers against the same ceiling.
type admissionRow struct{ from string }

func admissionRows(n int) []admissionRow {
	rows := make([]admissionRow, n)
	for i := range rows {
		rows[i].from = fmt.Sprintf("src-%06d", i)
	}
	return rows
}

// The admission is bounded in ROWS ADMITTED, never in store calls: it issues
// exactly one read for the whole key set, far past the 256-key cap the scoped
// physical projection enforces. Splitting the key set would multiply the
// incremental pass's documented constant number of logical store calls (see
// internal/resolver collectIncrementalFileFrontier and its batch_hotpaths
// guard) by ceil(keys/chunk) on the per-save hot path.
func TestAdmitIncomingRowsBoundedKeepsOneReadAndSharesOneCeiling(t *testing.T) {
	keys := make([]string, 600)
	for i := range keys {
		keys[i] = fmt.Sprintf("unresolved::Name%04d", i)
	}
	var requested [][]string
	read := func(chunk []string) map[string][]admissionRow {
		requested = append(requested, append([]string(nil), chunk...))
		out := make(map[string][]admissionRow, len(chunk))
		for _, key := range chunk {
			out[key] = admissionRows(1)
		}
		return out
	}
	rows, fact, err := AdmitIncomingRowsBounded(t.Context(), read, keys, nil)
	if err != nil {
		t.Fatalf("normal admission refused: %v", err)
	}
	if len(requested) != 1 {
		t.Fatalf("store reads = %d, want exactly 1 for %d keys", len(requested), len(keys))
	}
	if len(requested[0]) != len(keys) {
		t.Fatalf("single read carried %d of %d keys; the key set must not be split (scoped key cap %d)",
			len(requested[0]), len(keys), MaxBoundedAdjacencyKeys)
	}
	if fact.Dropped != 0 {
		t.Fatalf("admitted fact reports %d dropped rows, want 0", fact.Dropped)
	}
	if len(rows) != len(keys) || fact.Keys != len(keys) || fact.Inspected != len(keys) {
		t.Fatalf("admitted %d rows, fact %+v, want every key admitted once", len(rows), fact)
	}
	if fact.Refused || fact.Limit != MaxIncomingSourceCandidateRows {
		t.Fatalf("normal admission fact = %+v, want not refused at ceiling %d", fact, MaxIncomingSourceCandidateRows)
	}

	// Duplicate and empty keys never become extra reads or extra charges.
	requested = nil
	rows, fact, err = AdmitIncomingRowsBounded(t.Context(), read, []string{"a", "", "a", "b"}, nil)
	if err != nil || len(rows) != 2 || fact.Keys != 2 || fact.Inspected != 2 {
		t.Fatalf("dedup admission = %d rows, fact %+v, err %v", len(rows), fact, err)
	}

	// One budget passed to two admissions is one ceiling for both.
	budget := &IncomingSourceBudget{}
	half := MaxIncomingSourceCandidateRows/2 + 1
	bulk := func(chunk []string) map[string][]admissionRow {
		return map[string][]admissionRow{chunk[0]: admissionRows(half)}
	}
	if _, fact, err = AdmitIncomingRowsBounded(t.Context(), bulk, []string{"first"}, budget); err != nil {
		t.Fatalf("first half-ceiling admission refused: %v (%+v)", err, fact)
	}
	rows, fact, err = AdmitIncomingRowsBounded(t.Context(), bulk, []string{"second"}, budget)
	var limit *BoundedLocalizationLimitError
	if !errors.As(err, &limit) || rows != nil || !fact.Refused {
		t.Fatalf("shared budget did not refuse the second admission: rows=%d fact=%+v err=%v", len(rows), fact, err)
	}
	if limit.Resource != "incoming-source candidate inspections" || limit.Limit != MaxIncomingSourceCandidateRows {
		t.Fatalf("untyped refusal: %+v", limit)
	}
}

func TestAdmitIncomingRowsBoundedRefusesWholeBatchWithoutPartialAdmission(t *testing.T) {
	keys := make([]string, 600)
	for i := range keys {
		keys[i] = fmt.Sprintf("unresolved::Name%04d", i)
	}
	var reads int
	read := func(chunk []string) map[string][]admissionRow {
		reads++
		out := make(map[string][]admissionRow, len(chunk))
		for _, key := range chunk {
			// The adversarial shape: one common name carrying more parked
			// references than the whole query is allowed to inspect.
			if key == keys[0] {
				out[key] = admissionRows(MaxIncomingSourceCandidateRows + 1)
				continue
			}
			out[key] = admissionRows(1)
		}
		return out
	}
	rows, fact, err := AdmitIncomingRowsBounded(t.Context(), read, keys, nil)
	var limit *BoundedLocalizationLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("over-ceiling admission err = %v, want *BoundedLocalizationLimitError", err)
	}
	if rows != nil {
		t.Fatalf("refused admission returned %d partial keys; a partial admission is indistinguishable from a complete one", len(rows))
	}
	if !fact.Refused || fact.Limit != MaxIncomingSourceCandidateRows {
		t.Fatalf("completeness fact = %+v, want Refused at the ceiling", fact)
	}
	// The hole is reported whole: every row the batch carried, not the prefix
	// an early stop happened to reach. One adversarial key holds ceiling+1 rows
	// and the other 599 keys hold one each.
	wantInspected := MaxIncomingSourceCandidateRows + 1 + (len(keys) - 1)
	if fact.Inspected != wantInspected {
		t.Fatalf("inspected rows = %d, want the whole batch %d", fact.Inspected, wantInspected)
	}
	if fact.Dropped != fact.Inspected {
		t.Fatalf("dropped rows = %d, want the whole refused batch %d", fact.Dropped, fact.Inspected)
	}
	if reads != 1 {
		t.Fatalf("reads = %d, want exactly one batched read", reads)
	}

	reads = 0
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, fact, err = AdmitIncomingRowsBounded(ctx, read, keys, nil)
	if !errors.Is(err, context.Canceled) || rows != nil || reads != 0 {
		t.Fatalf("cancellation no longer precedes the read: rows=%d reads=%d err=%v", len(rows), reads, err)
	}
	if fact.Refused || fact.Dropped != 0 {
		t.Fatalf("cancellation reported as a bound hit: %+v", fact)
	}
}
