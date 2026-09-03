package store_sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const maskTestRepo = "repo"

func openMaskStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "generation_masks.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

// TestGenerationMaskRoundTrip proves each mask kind survives a write/read pair
// through the generation it was written at, values intact.
func TestGenerationMaskRoundTrip(t *testing.T) {
	derived := openMaskStore(t).AtGeneration(1)
	if derived == nil {
		t.Fatal("AtGeneration(1) returned nil")
	}

	masks := []FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipReplace},
		{RepoPrefix: maskTestRepo, FilePath: "repo/gone.go", Mode: OwnershipDelete},
	}
	if err := derived.SetFileMasks(masks); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	got, err := derived.FileMasks()
	if err != nil {
		t.Fatalf("FileMasks: %v", err)
	}
	if len(got) != 2 || got[0] != masks[0] || got[1] != masks[1] {
		t.Fatalf("FileMasks = %+v, want %+v", got, masks)
	}
	mode, ok, err := derived.FileMaskFor(maskTestRepo, "repo/gone.go")
	if err != nil || !ok || mode != OwnershipDelete {
		t.Fatalf("FileMaskFor(gone.go) = %q, %v, %v", mode, ok, err)
	}
	if _, ok, err := derived.FileMaskFor(maskTestRepo, "repo/unmentioned.go"); err != nil || ok {
		t.Fatalf("FileMaskFor on an unmasked path = %v, %v, want false", ok, err)
	}

	// A second write for the same key replaces rather than duplicating.
	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks re-upsert: %v", err)
	}
	if got, err = derived.FileMasks(); err != nil || len(got) != 2 || got[0].Mode != OwnershipDelete {
		t.Fatalf("FileMasks after re-upsert = %+v (err %v)", got, err)
	}

	if err := derived.SetNodeTombstones([]string{"repo/a.go::Gone", "repo/a.go::AlsoGone"}); err != nil {
		t.Fatalf("SetNodeTombstones: %v", err)
	}
	tombstones, err := derived.NodeTombstones()
	if err != nil || len(tombstones) != 2 || tombstones[0] != "repo/a.go::AlsoGone" {
		t.Fatalf("NodeTombstones = %v (err %v)", tombstones, err)
	}

	sources := []EdgeSourceMask{{SourceID: "repo/a.go::Caller", Mode: OwnershipReplace}}
	if err := derived.SetEdgeSourceMasks(sources); err != nil {
		t.Fatalf("SetEdgeSourceMasks: %v", err)
	}
	gotSources, err := derived.EdgeSourceMasks()
	if err != nil || len(gotSources) != 1 || gotSources[0] != sources[0] {
		t.Fatalf("EdgeSourceMasks = %+v (err %v)", gotSources, err)
	}

	producer := ProducerCompleteness{
		Producer: "resolver", State: ProducerStateIncomplete, Reason: "budget exhausted",
	}
	if err := derived.SetProducerState(producer); err != nil {
		t.Fatalf("SetProducerState: %v", err)
	}
	if err := derived.SetProducerState(ProducerCompleteness{
		Producer: "lsp", State: ProducerStateDisabledByConfig,
	}); err != nil {
		t.Fatalf("SetProducerState(lsp): %v", err)
	}
	states, err := derived.ProducerStates()
	if err != nil || len(states) != 2 {
		t.Fatalf("ProducerStates = %+v (err %v)", states, err)
	}
	if states[0].Producer != "lsp" || states[0].State != ProducerStateDisabledByConfig || states[0].Reason != "" {
		t.Fatalf("ProducerStates[0] = %+v", states[0])
	}
	if states[1] != producer {
		t.Fatalf("ProducerStates[1] = %+v, want %+v", states[1], producer)
	}
}

func TestSetProducerStatesValidatesBatchBeforeWriting(t *testing.T) {
	derived := openMaskStore(t).AtGeneration(1)
	invalid := []ProducerCompleteness{
		{Producer: "syntax", State: ProducerStateComplete},
		{Producer: "resolver", State: ProducerState("unknown")},
	}
	if err := derived.SetProducerStates(invalid); !errors.Is(err, ErrGenerationMaskInvalidValue) {
		t.Fatalf("SetProducerStates(invalid) = %v, want ErrGenerationMaskInvalidValue", err)
	}
	if states, err := derived.ProducerStates(); err != nil || len(states) != 0 {
		t.Fatalf("invalid producer batch wrote %+v (err %v)", states, err)
	}

	valid := []ProducerCompleteness{
		{Producer: "syntax", State: ProducerStateComplete},
		{Producer: "resolver", State: ProducerStateIncomplete, Reason: "closure truncated"},
	}
	if err := derived.SetProducerStates(valid); err != nil {
		t.Fatalf("SetProducerStates(valid): %v", err)
	}
	states, err := derived.ProducerStates()
	if err != nil || len(states) != len(valid) {
		t.Fatalf("ProducerStates = %+v (err %v), want %d rows", states, err, len(valid))
	}
}

// TestGenerationMaskWritesRefuseBaseHandle pins the typed refusal, including
// for an empty batch: a caller that never derived a handle must find out on the
// first call, not on the first row.
func TestGenerationMaskWritesRefuseBaseHandle(t *testing.T) {
	base := openMaskStore(t)
	if base.ViewGeneration() != baseViewGeneration {
		t.Fatalf("Open returned a handle at generation %d, want the base", base.ViewGeneration())
	}
	for name, write := range map[string]func() error{
		"SetFileMasks": func() error {
			return base.SetFileMasks([]FileMask{
				{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipReplace},
			})
		},
		"SetFileMasks_empty":      func() error { return base.SetFileMasks(nil) },
		"SetNodeTombstones":       func() error { return base.SetNodeTombstones([]string{"repo/a.go::Gone"}) },
		"SetNodeTombstones_empty": func() error { return base.SetNodeTombstones(nil) },
		"SetEdgeSourceMasks": func() error {
			return base.SetEdgeSourceMasks([]EdgeSourceMask{{SourceID: "x", Mode: OwnershipReplace}})
		},
		"SetEdgeSourceMasks_empty": func() error { return base.SetEdgeSourceMasks(nil) },
		"SetProducerState": func() error {
			return base.SetProducerState(ProducerCompleteness{Producer: "resolver", State: ProducerStateComplete})
		},
	} {
		if err := write(); !errors.Is(err, ErrMasksAtBaseGeneration) {
			t.Fatalf("%s at the base generation = %v, want ErrMasksAtBaseGeneration", name, err)
		}
	}

	// Reads at the base stay legal and simply report no claims.
	if masks, err := base.FileMasks(); err != nil || len(masks) != 0 {
		t.Fatalf("base FileMasks = %+v (err %v), want none", masks, err)
	}
	if err := base.ValidateGenerationMasks(); err != nil {
		t.Fatalf("base ValidateGenerationMasks = %v, want nil", err)
	}
}

// TestGenerationMaskRejectsInvalidValues pins the Go-side vocabulary check the
// plain TEXT columns delegate to, including the narrower edge-source set.
func TestGenerationMaskRejectsInvalidValues(t *testing.T) {
	derived := openMaskStore(t).AtGeneration(1)
	for name, write := range map[string]func() error{
		"unknown file mode": func() error {
			return derived.SetFileMasks([]FileMask{
				{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipMode("shadow")},
			})
		},
		"empty file path": func() error {
			return derived.SetFileMasks([]FileMask{{RepoPrefix: maskTestRepo, Mode: OwnershipReplace}})
		},
		"empty node id": func() error { return derived.SetNodeTombstones([]string{""}) },
		"delete on an edge source": func() error {
			return derived.SetEdgeSourceMasks([]EdgeSourceMask{
				{SourceID: "repo/a.go::Caller", Mode: OwnershipDelete},
			})
		},
		"empty source id": func() error {
			return derived.SetEdgeSourceMasks([]EdgeSourceMask{{Mode: OwnershipReplace}})
		},
		"unknown producer state": func() error {
			return derived.SetProducerState(ProducerCompleteness{Producer: "resolver", State: ProducerState("maybe")})
		},
		"empty producer": func() error {
			return derived.SetProducerState(ProducerCompleteness{State: ProducerStateComplete})
		},
	} {
		if err := write(); !errors.Is(err, ErrGenerationMaskInvalidValue) {
			t.Fatalf("%s = %v, want ErrGenerationMaskInvalidValue", name, err)
		}
	}
	// A rejected batch writes nothing at all.
	if masks, err := derived.FileMasks(); err != nil || len(masks) != 0 {
		t.Fatalf("FileMasks after rejected writes = %+v (err %v)", masks, err)
	}
}

// TestGenerationMaskBatchesChunk drives a batch past generationMaskChunk so the
// multi-statement path is exercised rather than assumed.
func TestGenerationMaskBatchesChunk(t *testing.T) {
	derived := openMaskStore(t).AtGeneration(1)
	const total = generationMaskChunk*2 + 17
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		ids = append(ids, fmt.Sprintf("repo/a.go::Gone%04d", i))
	}
	if err := derived.SetNodeTombstones(ids); err != nil {
		t.Fatalf("SetNodeTombstones: %v", err)
	}
	got, err := derived.NodeTombstones()
	if err != nil || len(got) != total {
		t.Fatalf("NodeTombstones returned %d rows (err %v), want %d", len(got), err, total)
	}
	if got[0] != ids[0] || got[total-1] != ids[total-1] {
		t.Fatalf("chunked write lost the boundary rows: first %q last %q", got[0], got[total-1])
	}
}

// TestValidateGenerationMasks covers the four combinations of the integrity
// rule, each against payload written at the same generation as the mask.
func TestValidateGenerationMasks(t *testing.T) {
	store := openMaskStore(t)
	derived := store.AtGeneration(1)

	// A file the generation extracted symbols from, a file it read that
	// produced none (a files row with node_count 0 — the explicit empty-file
	// marker), and a file it never touched.
	derived.AddBatch([]*graph.Node{{
		ID: "repo/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha",
		FilePath: "repo/a.go", RepoPrefix: maskTestRepo,
	}}, nil)
	if err := derived.SetFileMetas(maskTestRepo, []graph.FileMetaRow{
		{FilePath: "repo/a.go", ContentHash: "hash-a", Size: 10, NodeCount: 1},
		{FilePath: "repo/empty.go", ContentHash: "hash-empty", Size: 3, NodeCount: 0},
	}); err != nil {
		t.Fatalf("SetFileMetas: %v", err)
	}

	// A replace mask over each covered file, and a delete mask over the
	// uncovered one: the sound arrangement.
	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipReplace},
		{RepoPrefix: maskTestRepo, FilePath: "repo/empty.go", Mode: OwnershipReplace},
		{RepoPrefix: maskTestRepo, FilePath: "repo/removed.go", Mode: OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := derived.ValidateGenerationMasks(); err != nil {
		t.Fatalf("ValidateGenerationMasks on sound masks = %v", err)
	}

	// A replace mask over a path the generation carries nothing for.
	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/absent.go", Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(absent): %v", err)
	}
	err := derived.ValidateGenerationMasks()
	if !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("replace mask without payload = %v, want ErrGenerationMaskIntegrity", err)
	}

	// A delete mask over a path the generation still carries.
	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/absent.go", Mode: OwnershipDelete},
		{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks(delete over payload): %v", err)
	}
	if err := derived.ValidateGenerationMasks(); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("delete mask over live payload = %v, want ErrGenerationMaskIntegrity", err)
	}

	// The rule reads payload at the MASK's generation, not at any other: a
	// neighbouring generation carrying the same path cannot rescue this one's
	// replace mask.
	neighbour := store.AtGeneration(2)
	if err := neighbour.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/a.go", Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks at generation 2: %v", err)
	}
	if err := neighbour.ValidateGenerationMasks(); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("replace mask over another generation's payload = %v, want ErrGenerationMaskIntegrity", err)
	}
}

// TestValidateGenerationMasksProbesEdgesAndContent covers the rest of the
// payload a delete mask must not contradict: an edge whose call site sits in
// the deleted file, and a content document indexed at its path. Neither backs
// a replace mask, which still needs the generation to claim the file itself.
func TestValidateGenerationMasksProbesEdgesAndContent(t *testing.T) {
	store := openMaskStore(t)

	// A generation that re-emitted a masked source's edges keeps call sites in
	// a file it also claims to have deleted.
	edges := store.AtGeneration(1)
	edges.AddBatch(nil, []*graph.Edge{{
		From: "repo/a.go::Alpha", To: "repo/gone.go::Gone",
		Kind: graph.EdgeCalls, FilePath: "repo/gone.go", Line: 3,
	}})
	if err := edges.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/gone.go", Mode: OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks(edge): %v", err)
	}
	if err := edges.ValidateGenerationMasks(); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("delete mask over a carried edge = %v, want ErrGenerationMaskIntegrity", err)
	}

	// The same contradiction one table over: an indexed document at the path.
	content := store.AtGeneration(2)
	if err := content.AppendContent(maskTestRepo, []graph.ContentFTSItem{
		{NodeID: "repo/doc.md::0", FilePath: "repo/doc.md", Body: "text"},
	}); err != nil {
		t.Fatalf("AppendContent: %v", err)
	}
	if err := content.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/doc.md", Mode: OwnershipDelete},
	}); err != nil {
		t.Fatalf("SetFileMasks(content): %v", err)
	}
	if err := content.ValidateGenerationMasks(); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("delete mask over an indexed document = %v, want ErrGenerationMaskIntegrity", err)
	}

	// Turning the same masks into replace claims does not make them sound:
	// neither an edge nor a document is the generation claiming the file.
	if err := edges.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/gone.go", Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(edge replace): %v", err)
	}
	if err := edges.ValidateGenerationMasks(); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("replace mask backed only by an edge = %v, want ErrGenerationMaskIntegrity", err)
	}
	if err := content.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/doc.md", Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetFileMasks(content replace): %v", err)
	}
	if err := content.ValidateGenerationMasks(); !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("replace mask backed only by a document = %v, want ErrGenerationMaskIntegrity", err)
	}
}
