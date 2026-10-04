package store_sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// seedRawFileMask writes a mask row the public API refuses, through the same
// write transaction every mask write takes. It is the only way to stage a row
// a forward-dated binary could have left behind.
func seedRawFileMask(t *testing.T, derived *Store, filePath, mode string) {
	t.Helper()
	derived.writeMu.Lock()
	defer derived.writeMu.Unlock()
	tx, err := derived.beginWrite()
	if err != nil {
		t.Fatalf("begin write: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if _, err := tx.Exec(`INSERT OR REPLACE INTO generation_file_masks
  (view_gen, repo_prefix, file_path, ownership_mode) VALUES (?, ?, ?, ?)`,
		derived.viewGen, maskTestRepo, filePath, mode); err != nil {
		t.Fatalf("seed mask row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seeded mask row: %v", err)
	}
}

// TestContextMaskRoundTripsAndIsRefusedAtTheBase pins the new mode against the
// two rules every file mask already obeys: it survives the write/read pair
// with its value intact, and a base handle refuses to state it at all.
func TestContextMaskRoundTripsAndIsRefusedAtTheBase(t *testing.T) {
	store := openMaskStore(t)
	derived := store.AtGeneration(1)

	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/context.go", Mode: OwnershipContext},
	}); err != nil {
		t.Fatalf("SetFileMasks(context): %v", err)
	}
	mode, ok, err := derived.FileMaskFor(maskTestRepo, "repo/context.go")
	if err != nil || !ok || mode != OwnershipContext {
		t.Fatalf("FileMaskFor(context.go) = %q, %v, %v; want context, true, nil", mode, ok, err)
	}
	paths, err := derived.ContextMaskPaths()
	if err != nil || len(paths) != 1 || paths[0] != "repo/context.go" {
		t.Fatalf("ContextMaskPaths = %v (err %v), want [repo/context.go]", paths, err)
	}

	if err := store.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/context.go", Mode: OwnershipContext},
	}); !errors.Is(err, ErrMasksAtBaseGeneration) {
		t.Fatalf("context mask at the base generation = %v, want ErrMasksAtBaseGeneration", err)
	}
}

// TestContextMaskRefusedOverPayload is the gate-3 first-clause guard: a
// generation may declare a path read-only context only when it carries no
// payload for it. Each probe the delete arm uses is exercised on its own, so a
// dropped arm cannot hide behind a sibling.
func TestContextMaskRefusedOverPayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		arrange func(t *testing.T, derived *Store, path string)
	}{
		{
			name: "node",
			path: "repo/node.go",
			arrange: func(t *testing.T, derived *Store, path string) {
				derived.AddBatch([]*graph.Node{{
					ID: path + "::Alpha", Kind: graph.KindFunction, Name: "Alpha",
					FilePath: path, RepoPrefix: maskTestRepo,
				}}, nil)
			},
		},
		{
			name: "files row",
			path: "repo/meta.go",
			arrange: func(t *testing.T, derived *Store, path string) {
				if err := derived.SetFileMetas(maskTestRepo, []graph.FileMetaRow{
					{FilePath: path, ContentHash: "hash", Size: 4, NodeCount: 0},
				}); err != nil {
					t.Fatalf("SetFileMetas: %v", err)
				}
			},
		},
		{
			name: "edge recorded at the path",
			path: "repo/edge.go",
			arrange: func(t *testing.T, derived *Store, path string) {
				derived.AddBatch(nil, []*graph.Edge{{
					From: "repo/other.go::Caller", To: "repo/other.go::Callee",
					Kind: graph.EdgeCalls, FilePath: path, Line: 3,
				}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			derived := openMaskStore(t).AtGeneration(1)
			tc.arrange(t, derived, tc.path)
			if err := derived.SetFileMasks([]FileMask{
				{RepoPrefix: maskTestRepo, FilePath: tc.path, Mode: OwnershipContext},
			}); err != nil {
				t.Fatalf("SetFileMasks(context): %v", err)
			}
			err := derived.ValidateGenerationMasks()
			if !errors.Is(err, ErrGenerationMaskIntegrity) {
				t.Fatalf("context mask over %s payload = %v, want ErrGenerationMaskIntegrity", tc.name, err)
			}
			if !strings.Contains(err.Error(), "context masks claim nothing") {
				t.Fatalf("violation does not name the context rule: %v", err)
			}
		})
	}
}

// TestContextMaskValidOverAnUntouchedPath is the positive half: a generation
// that read a file and wrote nothing for it publishes with the claim intact.
func TestContextMaskValidOverAnUntouchedPath(t *testing.T) {
	derived := openMaskStore(t).AtGeneration(1)
	derived.AddBatch([]*graph.Node{{
		ID: "repo/changed.go::Alpha", Kind: graph.KindFunction, Name: "Alpha",
		FilePath: "repo/changed.go", RepoPrefix: maskTestRepo,
	}}, nil)
	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/changed.go", Mode: OwnershipReplace},
		{RepoPrefix: maskTestRepo, FilePath: "repo/context.go", Mode: OwnershipContext},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := derived.ValidateGenerationMasks(); err != nil {
		t.Fatalf("ValidateGenerationMasks on a sound context mask = %v", err)
	}
}

// TestContextMaskRefusesAnEdgeSourceClaim pins the contradiction the file-mask
// probe cannot see. The marker would make graphview serve an empty outgoing
// set for a node only the layer below still carries.
func TestContextMaskRefusesAnEdgeSourceClaim(t *testing.T) {
	derived := openMaskStore(t).AtGeneration(1)
	if err := derived.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/context.go", Mode: OwnershipContext},
	}); err != nil {
		t.Fatalf("SetFileMasks: %v", err)
	}
	if err := derived.SetEdgeSourceMasks([]EdgeSourceMask{
		{SourceID: "repo/context.go::Caller", Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetEdgeSourceMasks: %v", err)
	}
	err := derived.ValidateGenerationMasks()
	if !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("edge-source marker over a context path = %v, want ErrGenerationMaskIntegrity", err)
	}
	if !strings.Contains(err.Error(), "context path") {
		t.Fatalf("violation does not name the context path: %v", err)
	}

	// A marker for a pathless resolver stub is the case the marker exists for
	// and stays legal beside the same context mask.
	stub := openMaskStore(t).AtGeneration(1)
	if err := stub.SetFileMasks([]FileMask{
		{RepoPrefix: maskTestRepo, FilePath: "repo/context.go", Mode: OwnershipContext},
	}); err != nil {
		t.Fatalf("SetFileMasks(stub fixture): %v", err)
	}
	if err := stub.SetEdgeSourceMasks([]EdgeSourceMask{
		{SourceID: "repo::stdlib::fmt", Mode: OwnershipReplace},
	}); err != nil {
		t.Fatalf("SetEdgeSourceMasks(stub): %v", err)
	}
	if err := stub.ValidateGenerationMasks(); err != nil {
		t.Fatalf("pathless edge-source marker beside a context mask = %v, want nil", err)
	}
}

// TestValidateRefusesAnUnknownOwnershipMode proves the vocabulary column fails
// closed for a row a newer binary wrote. The write API cannot produce one, so
// the row is inserted directly — which is exactly the shape a forward-dated
// store hands this reader.
func TestValidateRefusesAnUnknownOwnershipMode(t *testing.T) {
	derived := openMaskStore(t).AtGeneration(1)
	seedRawFileMask(t, derived, "repo/future.go", "shadow")
	err := derived.ValidateGenerationMasks()
	if !errors.Is(err, ErrGenerationMaskIntegrity) {
		t.Fatalf("unknown ownership mode = %v, want ErrGenerationMaskIntegrity", err)
	}
	if !strings.Contains(err.Error(), "vocabulary") {
		t.Fatalf("violation does not name the vocabulary: %v", err)
	}
}

// TestPublishRefusesAContextMaskOverPayload proves the refusal is reachable
// where it matters. Every other case in this file calls the mask validator
// directly; this one goes through PublishPayloadGeneration — the entrypoint
// the sparse builder itself calls — so the guard cannot quietly become a
// test-only oracle. Both arms are checked from the same fixture: a context
// mask over a carried node refuses the publish, and a context mask over a path
// the generation genuinely left alone publishes.
func TestPublishRefusesAContextMaskOverPayload(t *testing.T) {
	begin := func(t *testing.T, store *Store, layerID string) (int64, *Store) {
		t.Helper()
		generationID, handle, err := store.BeginPayloadGeneration(context.Background(), PayloadGenerationRequest{
			OwnerKind: "dedicated_graph", GraphID: "publish-context", LayerID: layerID,
			GenerationKind: "dirty", TreeOID: "tree-" + layerID, CreatedAt: 1,
		})
		if err != nil {
			t.Fatalf("BeginPayloadGeneration(%s): %v", layerID, err)
		}
		return generationID, handle
	}

	t.Run("refused over payload", func(t *testing.T) {
		store := openMaskStore(t)
		generationID, handle := begin(t, store, "carried")
		const carried = "repo/carried.go"
		handle.AddBatch([]*graph.Node{{
			ID: carried + "::Alpha", Kind: graph.KindFunction, Name: "Alpha",
			FilePath: carried, RepoPrefix: maskTestRepo,
		}}, nil)
		if err := handle.SetFileMasks([]FileMask{
			{RepoPrefix: maskTestRepo, FilePath: carried, Mode: OwnershipContext},
		}); err != nil {
			t.Fatalf("SetFileMasks(context): %v", err)
		}
		err := store.PublishPayloadGeneration(context.Background(), generationID, 2)
		if !errors.Is(err, ErrGenerationMaskIntegrity) {
			t.Fatalf("PublishPayloadGeneration over a carried context path = %v, want ErrGenerationMaskIntegrity", err)
		}
		if !strings.Contains(err.Error(), "context masks claim nothing") {
			t.Fatalf("publish refusal does not name the context rule: %v", err)
		}
	})

	t.Run("published over an untouched path", func(t *testing.T) {
		store := openMaskStore(t)
		generationID, handle := begin(t, store, "untouched")
		const changed, read = "repo/changed.go", "repo/read.go"
		handle.AddBatch([]*graph.Node{{
			ID: changed + "::Alpha", Kind: graph.KindFunction, Name: "Alpha",
			FilePath: changed, RepoPrefix: maskTestRepo,
		}}, nil)
		if err := handle.SetFileMasks([]FileMask{
			{RepoPrefix: maskTestRepo, FilePath: changed, Mode: OwnershipReplace},
			{RepoPrefix: maskTestRepo, FilePath: read, Mode: OwnershipContext},
		}); err != nil {
			t.Fatalf("SetFileMasks: %v", err)
		}
		if err := store.PublishPayloadGeneration(context.Background(), generationID, 2); err != nil {
			t.Fatalf("PublishPayloadGeneration of a context-separated generation: %v", err)
		}
	})
}
