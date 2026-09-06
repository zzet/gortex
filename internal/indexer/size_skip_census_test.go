package indexer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/yaml.v3"
)

type sizeCensusFixture struct {
	t            *testing.T
	root, dbPath string
	store        *store_sqlite.Store
	idx          *Indexer
	mtime        time.Time
}

func newSizeCensusFixture(t *testing.T, limit int64) *sizeCensusFixture {
	t.Helper()
	f := &sizeCensusFixture{t: t, root: t.TempDir(), dbPath: filepath.Join(t.TempDir(), "graph.sqlite")}
	t.Cleanup(f.close)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "main.go"), []byte("package fixture\nfunc Main() {}\n"), 0600))
	writeSizeCensusGIF(t, filepath.Join(f.root, "demo.gif"), 512)
	info, err := os.Stat(filepath.Join(f.root, "demo.gif"))
	require.NoError(t, err)
	f.mtime = info.ModTime()
	f.open(limit)
	_, err = f.idx.IndexCtx(context.Background(), f.root)
	require.NoError(t, err)
	f.idx.RunDeferredPasses(context.Background())
	return f
}

func writeSizeCensusGIF(t *testing.T, path string, size int) {
	t.Helper()
	var body bytes.Buffer
	im := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	im.SetColorIndex(1, 1, 1)
	require.NoError(t, gif.Encode(&body, im, nil))
	require.LessOrEqual(t, body.Len(), size)
	data := append(body.Bytes(), make([]byte, size-body.Len())...)
	// A real GIF, with permitted trailing padding; eligible re-indexes exercise
	// the actual ImageAssetExtractor, not only a synthetic skip marker.
	_, err := gif.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
}

func (f *sizeCensusFixture) open(limit int64) {
	f.t.Helper()
	var err error
	f.store, err = store_sqlite.Open(f.dbPath)
	require.NoError(f.t, err)
	registry := newTestRegistry()
	registry.Register(languages.NewImageAssetExtractor())
	f.idx = New(f.store, registry, config.IndexConfig{MaxFileSize: limit}, zap.NewNop())
	f.idx.SetRepoPrefix("repo")
	f.idx.SetDeferResolve(true)
	f.idx.SetDeferGlobalPasses(true)
	f.idx.storeRootPath(f.root)
	f.idx.SetFileMtimes(f.store.LoadFileMtimes("repo"))
}

func (f *sizeCensusFixture) close() {
	if f.idx != nil {
		f.idx.Close()
		f.idx = nil
	}
	if f.store != nil {
		require.NoError(f.t, f.store.Close())
		f.store = nil
	}
}

func (f *sizeCensusFixture) reopen(limit int64) { f.close(); f.open(limit) }

func (f *sizeCensusFixture) fileNode() *graph.Node {
	f.t.Helper()
	node := f.store.GetNodesByIDs([]string{"repo/demo.gif"})["repo/demo.gif"]
	require.NotNil(f.t, node)
	return node
}

// Equal-mtime cases deliberately establish a known receipt after actual cold
// indexing, so the older missing-cold-receipt bug cannot mask policy staleness.
func (f *sizeCensusFixture) establishKnownReceipt() {
	f.t.Helper()
	require.NoError(f.t, f.store.BulkSetFileMtimes("repo", map[string]int64{"demo.gif": f.mtime.UnixNano()}))
	f.idx.SetFileMtimes(f.store.LoadFileMtimes("repo"))
}

func (f *sizeCensusFixture) census(wantChanged, wantDeleted []string, detected int) []string {
	f.t.Helper()
	changed, deleted, gotDetected, err := f.idx.changedSinceMtimesCensus(f.root)
	require.NoError(f.t, err)
	sort.Strings(changed)
	sort.Strings(deleted)
	require.ElementsMatch(f.t, wantChanged, changed)
	require.ElementsMatch(f.t, wantDeleted, deleted)
	require.Equal(f.t, detected, gotDetected)
	return changed
}

func (f *sizeCensusFixture) applyCensus(changed []string) {
	f.t.Helper()
	// This is the existing receipt-aware incremental API used by Reconcile's
	// explicit census frontier; policy changes may have an unchanged mtime.
	_, _, _, err := f.idx.incrementalReindexPathsWithReceiptMode(f.root, changed,
		incrementalPathMode{forceExplicitFiles: true, detectDeletions: true})
	require.NoError(f.t, err)
}

func TestSizeSkipCensusColdGIFRepeatedRestartNoop(t *testing.T) {
	f := newSizeCensusFixture(t, 128)
	stub := f.fileNode()
	require.Equal(t, true, stub.Meta["skipped_due_to_size"])
	require.Equal(t, "size", stub.Meta["skip_reason"])
	require.EqualValues(t, 512, stub.Meta["file_size_bytes"])
	require.EqualValues(t, 128, stub.Meta["max_file_size_bytes"])
	seeded := []*graph.Edge{
		{From: "repo/main.go", To: "repo/demo.gif", Kind: "co_change", FilePath: "repo/main.go"},
		{From: "repo/demo.gif", To: "repo/main.go", Kind: "co_change", FilePath: "repo/demo.gif"},
	}
	f.store.AddBatch(nil, seeded)
	stored := f.store.GetOutEdgesByNodeIDs([]string{"repo/main.go", "repo/demo.gif"})
	for _, want := range seeded {
		found := 0
		for _, got := range stored[want.From] {
			if got.From == want.From && got.To == want.To && got.Kind == want.Kind {
				found++
			}
		}
		require.Equal(t, 1, found, "seeded co_change edge %s -> %s must exist before parity snapshot", want.From, want.To)
	}
	require.Equal(t, f.mtime.UnixNano(), f.store.LoadFileMtimes("repo")["demo.gif"], "cold graph publication must persist the actual oversized GIF receipt")
	before := sizeCensusGraphRows(t, f.store)
	for restart := 0; restart < 2; restart++ {
		f.reopen(128)
		f.census(nil, nil, 2)
		result, err := f.idx.incrementalReindexPathsMode(f.root, []string{"main.go", "demo.gif"}, incrementalPathMode{})
		require.NoError(t, err)
		require.Zero(t, result.StaleFileCount)
		require.Equal(t, before, sizeCensusGraphRows(t, f.store))
	}
}

func TestSizeSkipCensusEqualMtimePolicyTransitions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after int64
		wantSkip      bool
	}{
		{"cap_increase_admits", 128, 1024, false},
		{"cap_disabled_admits", 128, 0, false},
		{"cap_decrease_skips", 1024, 128, true},
		{"still_oversize_new_cap", 128, 256, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSizeCensusFixture(t, tc.before)
			f.establishKnownReceipt()
			f.reopen(tc.after)
			changed := f.census([]string{"demo.gif"}, nil, 2)
			f.applyCensus(changed)
			node := f.fileNode()
			if tc.wantSkip {
				require.Equal(t, true, node.Meta["skipped_due_to_size"])
				require.EqualValues(t, tc.after, node.Meta["max_file_size_bytes"])
			} else {
				require.NotEqual(t, true, node.Meta["skipped_due_to_size"])
			}
			require.Equal(t, f.mtime.UnixNano(), f.store.LoadFileMtimes("repo")["demo.gif"])
			f.reopen(tc.after)
			f.census(nil, nil, 2)
		})
	}
}

func TestSizeSkipCensusEqualMtimeSizeChange(t *testing.T) {
	for _, size := range []int{768, 96} {
		name := "still_oversize"
		if size == 96 {
			name = "shrunk_under_cap"
		}
		t.Run(name, func(t *testing.T) {
			f := newSizeCensusFixture(t, 128)
			f.establishKnownReceipt()
			path := filepath.Join(f.root, "demo.gif")
			writeSizeCensusGIF(t, path, size)
			require.NoError(t, os.Chtimes(path, f.mtime, f.mtime))
			f.reopen(128)
			changed := f.census([]string{"demo.gif"}, nil, 2)
			f.applyCensus(changed)
			if size > 128 {
				require.EqualValues(t, size, f.fileNode().Meta["file_size_bytes"])
			} else {
				require.NotEqual(t, true, f.fileNode().Meta["skipped_due_to_size"])
			}
			f.reopen(128)
			f.census(nil, nil, 2)
		})
	}
}

func TestSizeSkipCensusMissingOrInvalidStubCannotCertifyReceipt(t *testing.T) {
	for _, name := range []string{"missing", "wrong_kind", "false_skip", "wrong_reason", "wrong_size", "wrong_limit", "missing_size", "missing_limit", "nonnumeric_size"} {
		t.Run(name, func(t *testing.T) {
			f := newSizeCensusFixture(t, 128)
			node := f.fileNode()
			if name == "missing" {
				f.store.EvictFile("repo/demo.gif")
			} else {
				copyNode := *node
				copyNode.Meta = make(map[string]any, len(node.Meta))
				for key, value := range node.Meta {
					copyNode.Meta[key] = value
				}
				switch name {
				case "wrong_kind":
					copyNode.Kind = graph.KindFunction
				case "false_skip":
					copyNode.Meta["skipped_due_to_size"] = false
				case "wrong_reason":
					copyNode.Meta["skip_reason"] = "unsupported"
				case "wrong_size":
					copyNode.Meta["file_size_bytes"] = int64(511)
				case "wrong_limit":
					copyNode.Meta["max_file_size_bytes"] = int64(127)
				case "missing_size":
					delete(copyNode.Meta, "file_size_bytes")
				case "missing_limit":
					delete(copyNode.Meta, "max_file_size_bytes")
				case "nonnumeric_size":
					copyNode.Meta["file_size_bytes"] = "512"
				}
				f.store.AddBatch([]*graph.Node{&copyNode}, nil)
			}
			f.establishKnownReceipt()
			f.reopen(128)
			changed := f.census([]string{"demo.gif"}, nil, 2)
			f.applyCensus(changed)
			require.Equal(t, graph.KindFile, f.fileNode().Kind)
			require.Equal(t, true, f.fileNode().Meta["skipped_due_to_size"])
			f.reopen(128)
			f.census(nil, nil, 2)
		})
	}
}

func TestSizeSkipCensusDeletionRetainsDetection(t *testing.T) {
	f := newSizeCensusFixture(t, 128)
	f.establishKnownReceipt()
	f.reopen(128)
	require.NoError(t, os.Remove(filepath.Join(f.root, "demo.gif")))
	changed := f.census(nil, []string{"demo.gif"}, 1)
	f.applyCensus(changed)
	require.NotContains(t, f.store.LoadFileMtimes("repo"), "demo.gif")
	require.Nil(t, f.store.GetNodesByIDs([]string{"repo/demo.gif"})["repo/demo.gif"])
	f.reopen(128)
	f.census(nil, nil, 1)
}

func TestSizeSkipCensusMalformedMarkerStillRequiresEligibleParse(t *testing.T) {
	for _, malformed := range []string{"false_flag", "string_flag", "wrong_reason"} {
		t.Run(malformed, func(t *testing.T) {
			f := newSizeCensusFixture(t, 128)
			stub := *f.fileNode()
			stub.Meta = make(map[string]any)
			for key, value := range f.fileNode().Meta {
				stub.Meta[key] = value
			}
			switch malformed {
			case "false_flag":
				stub.Meta["skipped_due_to_size"] = false
			case "string_flag":
				stub.Meta["skipped_due_to_size"] = "true"
			case "wrong_reason":
				stub.Meta["skip_reason"] = "unsupported"
			}
			f.store.AddBatch([]*graph.Node{&stub}, nil)
			f.establishKnownReceipt()
			f.reopen(1024)
			changed := f.census([]string{"demo.gif"}, nil, 2)
			f.applyCensus(changed)
			// Require evidence of real asset extraction, not merely that the
			// malformed old flag was already unequal to boolean true.
			parsedAsset := false
			for _, node := range f.store.GetRepoNodes("repo") {
				if node.FilePath == "repo/demo.gif" && node.Kind != graph.KindFile {
					parsedAsset = true
				}
			}
			require.True(t, parsedAsset, "eligible image must be parsed despite malformed old skip marker")
			f.reopen(1024)
			f.census(nil, nil, 2)
		})
	}
}

func TestSizeSkipCensusForeignRepoOrGenerationCannotSupplyStub(t *testing.T) {
	for _, scope := range []string{"sibling_repo", "other_generation"} {
		t.Run(scope, func(t *testing.T) {
			f := newSizeCensusFixture(t, 128)
			foreign := *f.fileNode()
			foreign.ID = "foreign-size-stub"
			if scope == "sibling_repo" {
				foreign.RepoPrefix = "sibling"
			}
			// Preserve the colliding FilePath deliberately: repository/generation
			// boundaries must be applied before path-key projection.
			f.store.EvictFile("repo/demo.gif")
			f.store.AddBatch([]*graph.Node{&foreign}, nil)
			f.establishKnownReceipt()
			f.close()
			if scope == "other_generation" {
				// Test-only database, Store closed; metadata encoded by real AddBatch.
				db, err := sql.Open("sqlite", f.dbPath)
				require.NoError(t, err)
				result, err := db.Exec(`UPDATE nodes SET view_gen = 7 WHERE id = ?`, foreign.ID)
				require.NoError(t, err)
				rows, err := result.RowsAffected()
				require.NoError(t, err)
				require.EqualValues(t, 1, rows)
				require.NoError(t, db.Close())
			}
			f.open(128)
			f.census([]string{"demo.gif"}, nil, 2)
		})
	}
}

func TestSizeSkipCensusKnownStubWorkspaceDoesNotHideRepoFiles(t *testing.T) {
	for _, workspace := range []string{"", "current-workspace", "old-workspace"} {
		name := workspace
		if name == "" {
			name = "legacy_blank"
		}
		t.Run(name, func(t *testing.T) {
			f := newSizeCensusFixture(t, 128)
			stub := *f.fileNode()
			stub.WorkspaceID = workspace
			f.store.AddBatch([]*graph.Node{&stub}, nil)
			f.establishKnownReceipt()
			f.reopen(128)
			f.idx.SetWorkspaceID("current-workspace")
			// Repository-wide census must recognize a legacy or reassigned
			// workspace's persisted stub, without pruning its durable receipt.
			f.census(nil, nil, 2)
			result, err := f.idx.incrementalReindexPathsMode(f.root,
				[]string{"main.go", "demo.gif"}, incrementalPathMode{})
			require.NoError(t, err)
			require.Zero(t, result.StaleFileCount)
			require.Equal(t, f.mtime.UnixNano(), f.store.LoadFileMtimes("repo")["demo.gif"])
		})
	}
}

func TestSizeSkipCensusReconcileUsesExplicitPolicyFrontier(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "mtime"
		if enabled {
			name = "merkle"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("GORTEX_MERKLE", "")
			f := newSizeCensusFixture(t, 1024)
			// Four unchanged Go files plus one GIF keep a single policy transition
			// below the 40% full-retrack threshold. Even if the local configuration
			// file is admitted, two changes among six files remain below that limit.
			for i := 1; i <= 3; i++ {
				body := fmt.Sprintf("package fixture\nfunc Stable%d() {}\n", i)
				require.NoError(t, os.WriteFile(filepath.Join(f.root, fmt.Sprintf("stable%d.go", i)), []byte(body), 0600))
			}
			writeConfig := func(limit int64) {
				cfg := config.Default()
				cfg.Index.MaxFileSize = limit
				cfg.Index.Merkle = enabled
				encoded, err := yaml.Marshal(cfg)
				require.NoError(t, err)
				// Marshal the real configuration's tags; no guessed YAML key names.
				require.NoError(t, os.WriteFile(filepath.Join(f.root, ".gortex.yaml"), encoded, 0600))
			}
			writeConfig(1024)
			f.idx.config.Merkle = enabled
			require.Equal(t, enabled, f.idx.merkleEnabled())
			_, err := f.idx.IndexCtx(context.Background(), f.root)
			require.NoError(t, err)
			f.idx.RunDeferredPasses(context.Background())
			f.establishKnownReceipt()
			if enabled {
				encoded, err := os.ReadFile(merkleTreeFile(f.root))
				require.NoError(t, err)
				var baseline map[string]any
				require.NoError(t, json.Unmarshal(encoded, &baseline))
				files, ok := baseline["files"].(map[string]any)
				require.True(t, ok, "test-created baseline must contain its files map")
				leaf, ok := files["demo.gif"].(map[string]any)
				require.True(t, ok, "cold eligible GIF must have an actual leaf before cap decrease")
				hash, ok := leaf["hash"].(string)
				require.True(t, ok)
				require.Len(t, hash, 64)
			}
			require.GreaterOrEqual(t, len(f.store.LoadFileMtimes("repo")), 5)
			require.NotEqual(t, true, f.fileNode().Meta["skipped_due_to_size"])

			// A full retrack would lose this edge between unchanged files; a scoped
			// GIF refresh must leave it intact. This distinguishes the two routes
			// without binding the test to diagnostic log wording.
			unchanged := &graph.Edge{From: "repo/stable1.go", To: "repo/stable2.go",
				Kind: "co_change", FilePath: "repo/stable1.go", Line: 777}
			f.store.AddBatch(nil, []*graph.Edge{unchanged})
			assertUnchangedEdge := func() {
				edges := f.store.GetOutEdgesByNodeIDs([]string{unchanged.From})[unchanged.From]
				found := 0
				for _, edge := range edges {
					if edge.From == unchanged.From && edge.To == unchanged.To && edge.Kind == unchanged.Kind && edge.Line == unchanged.Line {
						found++
					}
				}
				require.Equal(t, 1, found, "scoped policy refresh must retain unchanged-file edge")
			}
			assertUnchangedEdge()

			entry := config.RepoEntry{Path: f.root, Name: "repo"}
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			global := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
			global.SetConfigPath(configPath)
			require.NoError(t, global.Save())
			reconcile := func(limit int64, wantStale bool) {
				f.reopen(limit)
				f.idx.Close()
				f.idx = nil
				manager, err := config.NewConfigManager(configPath)
				require.NoError(t, err)
				registry := newTestRegistry()
				registry.Register(languages.NewImageAssetExtractor())
				mi := NewMultiIndexer(f.store, registry, search.NewNull(), manager, zap.NewNop())
				// New MI is essential: already-registered repositories short-circuit.
				result, err := mi.ReconcileRepoCtx(context.Background(), entry, f.store.LoadFileMtimes("repo"))
				f.idx = mi.indexers["repo"]
				require.NoError(t, err)
				require.NotNil(t, result)
				require.NotNil(t, f.idx)
				require.EqualValues(t, limit, f.idx.config.MaxFileSize, "real repo config override must have loaded")
				require.Equal(t, enabled, f.idx.merkleEnabled())
				if wantStale {
					require.Positive(t, result.StaleFileCount)
				} else {
					require.Zero(t, result.StaleFileCount)
				}
				if limit < 512 {
					require.Equal(t, true, f.fileNode().Meta["skipped_due_to_size"])
					require.EqualValues(t, limit, f.fileNode().Meta["max_file_size_bytes"])
				} else {
					require.NotEqual(t, true, f.fileNode().Meta["skipped_due_to_size"])
				}
				require.Equal(t, f.mtime.UnixNano(), f.store.LoadFileMtimes("repo")["demo.gif"])
				assertUnchangedEdge()
			}
			writeConfig(128)
			reconcile(128, true)
			reconcile(128, false)
			writeConfig(1024)
			reconcile(1024, true)
			reconcile(1024, false)
		})
	}
}

func sizeCensusGraphRows(t *testing.T, store graph.Store) map[string]string {
	t.Helper()
	rows := make(map[string]string)
	var ids []string
	for _, node := range store.AllNodes() {
		body, err := json.Marshal(node)
		require.NoError(t, err)
		rows["node:"+node.ID] = string(body)
		ids = append(ids, node.ID)
	}
	for _, edges := range store.GetOutEdgesByNodeIDs(ids) {
		for _, edge := range edges {
			body, err := json.Marshal(edge)
			require.NoError(t, err)
			rows["edge:"+string(body)] = string(body)
		}
	}
	return rows
}

func TestSizeSkipReceiptPublicationDoesNotRequireReadableContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "unreadable.gif")
	var encoded bytes.Buffer
	picture := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	picture.SetColorIndex(1, 1, 1)
	require.NoError(t, gif.Encode(&encoded, picture, nil))
	const size = 512
	const limit = 128
	require.Less(t, encoded.Len(), size)
	content := append(encoded.Bytes(), make([]byte, size-encoded.Len())...)
	_, err := gif.Decode(bytes.NewReader(content))
	require.NoError(t, err, "fixture must be an actual GIF, not a fake extension")
	require.NoError(t, os.WriteFile(path, content, 0600))
	t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0600)) })
	require.NoError(t, os.Chmod(path, 0000))
	// Root/elevated readers and some platforms ignore permission bits. Such
	// environments cannot prove this property through a chmod fixture.
	if _, err = os.ReadFile(path); err == nil {
		t.Skip("current platform/user can read mode-000 files; content-read prohibition is not exercised")
	}
	require.ErrorIs(t, err, fs.ErrPermission)
	info, err := os.Stat(path)
	require.NoError(t, err, "metadata remains available without content permission")
	require.True(t, info.Mode().IsRegular())
	require.EqualValues(t, size, info.Size())
	t.Log("permission preflight exercised: content read denied, metadata stat succeeds")

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	registry := newTestRegistry()
	registry.Register(languages.NewImageAssetExtractor())
	idx := New(store, registry, config.IndexConfig{MaxFileSize: limit}, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.SetDeferResolve(true)
	idx.SetDeferGlobalPasses(true)
	t.Cleanup(idx.Close)
	result, err := idx.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result.FailedFiles, "deliberate size skip must not become a read failure")
	require.Empty(t, idx.fileIndexFailurePaths())
	stub := store.GetNodesByIDs([]string{"repo/unreadable.gif"})["repo/unreadable.gif"]
	require.NotNil(t, stub, "real cold graph publication must contain the skip stub")
	require.Equal(t, graph.KindFile, stub.Kind)
	require.Equal(t, true, stub.Meta["skipped_due_to_size"])
	require.Equal(t, "size", stub.Meta["skip_reason"])
	require.EqualValues(t, size, stub.Meta["file_size_bytes"])
	require.EqualValues(t, limit, stub.Meta["max_file_size_bytes"])
	_, err = os.ReadFile(path)
	require.ErrorIs(t, err, fs.ErrPermission, "indexing must not change file permissions")
	// This assertion is after the actual IndexCtx publication boundary and
	// before any deferred pass. No file bytes are supplied to the Indexer.
	require.Equal(t, info.ModTime().UnixNano(), store.LoadFileMtimes("repo")["unreadable.gif"],
		"a successfully published size skip must receive a durable metadata-only receipt")
	require.Equal(t, info.ModTime().UnixNano(), idx.publishFileMtimes()["unreadable.gif"])
}

func sizeFailureBoundaryWriteGIF(t *testing.T, path string, size int) {
	t.Helper()
	var encoded bytes.Buffer
	picture := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	picture.SetColorIndex(1, 1, 1)
	require.NoError(t, gif.Encode(&encoded, picture, nil))
	require.Less(t, encoded.Len(), size)
	content := append(encoded.Bytes(), make([]byte, size-encoded.Len())...)
	_, err := gif.Decode(bytes.NewReader(content))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, content, 0600))
}

// This tests the real durable receipt finalizers, not the raw IndexCtx call
// site. It deliberately supplies the publication outcome/captured version at
// that boundary. The actual IndexCtx tests below separately exercise the
// publication wiring through a source-verified log callback; no arbitrary
// AddBatch callback is used as a substitute for that boundary.
func TestSizeSkipFailureBoundaryRemovesOldReceiptAndRetries(t *testing.T) {
	for _, scenario := range []string{
		"publication_failed",
		"publication_cancelled",
		"same_mtime_size_changed",
		"same_mtime_same_size_file_replaced",
	} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			gifPath := filepath.Join(root, "demo.gif")
			const oldSize = 512
			const limit = 128
			sizeFailureBoundaryWriteGIF(t, gifPath, oldSize)
			capturedInfo, err := os.Stat(gifPath)
			require.NoError(t, err)
			oldMtime := capturedInfo.ModTime().UnixNano()
			peerPath := filepath.Join(root, "durable-only.go")
			require.NoError(t, os.WriteFile(peerPath, []byte("package fixture\n"), 0600))
			peerInfo, err := os.Stat(peerPath)
			require.NoError(t, err)
			peerMtime := peerInfo.ModTime().UnixNano()
			dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
			var store *store_sqlite.Store
			var idx *Indexer
			closeFixture := func() {
				if idx != nil {
					idx.Close()
					idx = nil
				}
				if store != nil {
					require.NoError(t, store.Close())
					store = nil
				}
			}
			t.Cleanup(closeFixture)
			openFixture := func() {
				var err error
				store, err = store_sqlite.Open(dbPath)
				require.NoError(t, err)
				registry := newTestRegistry()
				registry.Register(languages.NewImageAssetExtractor())
				idx = New(store, registry, config.IndexConfig{MaxFileSize: limit}, zap.NewNop())
				idx.SetRepoPrefix("repo")
				idx.storeRootPath(root)
			}
			openFixture()
			store.AddBatch([]*graph.Node{{
				ID: "repo/demo.gif", Kind: graph.KindFile, RepoPrefix: "repo", FilePath: "repo/demo.gif",
				Meta: map[string]any{
					"skipped_due_to_size": true, "skip_reason": "size",
					"file_size_bytes": int64(oldSize), "max_file_size_bytes": int64(limit),
				},
			}}, nil)
			require.NotNil(t, store.GetNodesByIDs([]string{"repo/demo.gif"})["repo/demo.gif"])
			require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{
				"demo.gif": oldMtime, "durable-only.go": peerMtime,
			}))
			// The unrelated key is intentionally absent from this Indexer's
			// map. A fallback replacement from local state must not prune it.
			idx.SetFileMtimes(map[string]int64{"demo.gif": oldMtime})
			receipt := idx.coldSizeSkipReceipt(gifPath, capturedInfo)
			require.Equal(t, "demo.gif", receipt.mtimeKey)
			require.True(t, receipt.readVersion.valid)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			published := true
			var publicationErr error
			switch scenario {
			case "publication_failed":
				published = false
				publicationErr = errors.New("injected final graph publication failure")
			case "publication_cancelled":
				cancel()
			case "same_mtime_size_changed":
				sizeFailureBoundaryWriteGIF(t, gifPath, 768)
				require.NoError(t, os.Chtimes(gifPath, capturedInfo.ModTime(), capturedInfo.ModTime()))
				currentInfo, err := os.Stat(gifPath)
				require.NoError(t, err)
				require.Equal(t, oldMtime, currentInfo.ModTime().UnixNano())
				require.NotEqual(t, capturedInfo.Size(), currentInfo.Size())
			case "same_mtime_same_size_file_replaced":
				replacement := filepath.Join(root, "replacement.gif")
				sizeFailureBoundaryWriteGIF(t, replacement, oldSize)
				require.NoError(t, os.Chtimes(replacement, capturedInfo.ModTime(), capturedInfo.ModTime()))
				require.NoError(t, os.Remove(gifPath))
				require.NoError(t, os.Rename(replacement, gifPath))
				currentInfo, err := os.Stat(gifPath)
				require.NoError(t, err)
				require.Equal(t, oldMtime, currentInfo.ModTime().UnixNano())
				require.Equal(t, capturedInfo.Size(), currentInfo.Size())
				require.False(t, os.SameFile(capturedInfo, currentInfo), "fixture must actually replace the file identity")
			}

			candidateMtimes := map[string]int64{}
			hadFailure := idx.finishColdSizeSkips(ctx, []fileReadReceipt{receipt}, candidateMtimes, published)
			require.True(t, hadFailure, "failure must suppress authoritative census pruning")
			require.NotContains(t, candidateMtimes, "demo.gif", "rejected version must not enter the candidate census")
			require.NotContains(t, idx.publishFileMtimes(), "demo.gif")
			require.Equal(t, map[string]int64{"durable-only.go": peerMtime}, store.LoadFileMtimes("repo"))
			// Exercise the next real finalizer too: forwarding hadFailure must
			// not turn the now-empty candidate map into authoritative deletion
			// of an unrelated persisted-only row.
			idx.finishColdIndexCensus(ctx, &IndexResult{}, publicationErr, candidateMtimes, nil, hadFailure)
			require.Equal(t, map[string]int64{"durable-only.go": peerMtime}, store.LoadFileMtimes("repo"))
			closeFixture()
			openFixture()
			idx.SetFileMtimes(store.LoadFileMtimes("repo"))
			idx.loadFileIndexFailures()
			require.NotContains(t, store.LoadFileMtimes("repo"), "demo.gif")
			require.Equal(t, peerMtime, store.LoadFileMtimes("repo")["durable-only.go"])
			changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
			require.NoError(t, err)
			require.Equal(t, []string{"demo.gif"}, changed, "reopened census must retry the rejected GIF receipt")
			require.Empty(t, deleted)
			require.Equal(t, 2, detected)
		})
	}
}

// The exact raw IndexCtx source logs "indexer: parse subphases" after
// emitSizeSkipNodes and construction of the candidate census, before vector /
// final graph publication and the receipt finalizers. A synchronous zap hook
// at that message therefore changes the file or cancels the actual IndexCtx
// AFTER the walk captured its size-skip version. No sleeps, production hooks,
// or manually supplied receipt/completion state are involved in these cases.
func TestSizeSkipFailureBoundaryIndexCtxWiring(t *testing.T) {
	for _, scenario := range []string{"cancel_after_capture", "same_mtime_size_change_after_capture"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			gifPath := filepath.Join(root, "demo.gif")
			const oldSize = 512
			const limit = 128
			sizeFailureBoundaryWriteGIF(t, gifPath, oldSize)
			capturedInfo, err := os.Stat(gifPath)
			require.NoError(t, err)
			oldMtime := capturedInfo.ModTime().UnixNano()
			peerPath := filepath.Join(root, "durable-only.go")
			require.NoError(t, os.WriteFile(peerPath, []byte("package fixture\n"), 0600))
			peerInfo, err := os.Stat(peerPath)
			require.NoError(t, err)
			peerMtime := peerInfo.ModTime().UnixNano()
			dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
			var idx *Indexer
			var store *store_sqlite.Store
			t.Cleanup(func() {
				if idx != nil {
					idx.Close()
				}
				if store != nil {
					require.NoError(t, store.Close())
				}
			})
			store, err = store_sqlite.Open(dbPath)
			require.NoError(t, err)
			newIndexer := func(logger *zap.Logger) *Indexer {
				registry := newTestRegistry()
				registry.Register(languages.NewImageAssetExtractor())
				result := New(store, registry, config.IndexConfig{MaxFileSize: limit}, logger)
				result.SetRepoPrefix("repo")
				result.SetDeferResolve(true)
				result.SetDeferGlobalPasses(true)
				result.storeRootPath(root)
				return result
			}
			// Establish a real previous graph, not only a synthetic file row.
			// Seed the old receipt explicitly so this fixture also works before
			// the separate missing-cold-receipt fix has landed.
			idx = newIndexer(zap.NewNop())
			_, err = idx.IndexCtx(context.Background(), root)
			require.NoError(t, err)
			idx.Close()
			idx = nil
			require.NotNil(t, store.GetNodesByIDs([]string{"repo/demo.gif"})["repo/demo.gif"])
			require.NoError(t, store.BulkSetFileMtimes("repo", map[string]int64{
				"demo.gif": oldMtime, "durable-only.go": peerMtime,
			}))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callbackHits atomic.Int32
			var callbackErr error
			var observedKind graph.NodeKind
			var observedSkip, observedSize, observedLimit any
			core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.InfoLevel)
			logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
				if entry.Message != "indexer: parse subphases" {
					return nil
				}
				if callbackHits.Add(1) != 1 {
					callbackErr = errors.New("publication callback unexpectedly ran more than once")
					return nil
				}
				if ctx.Err() != nil {
					callbackErr = errors.New("context was cancelled before the intended publication seam")
					return nil
				}
				// Read the active graph: on a shadow path it has not drained
				// to the durable Store yet. This proves size-node emission has
				// occurred without assuming a specific publication backend.
				stub := idx.graph.GetNodesByIDs([]string{"repo/demo.gif"})["repo/demo.gif"]
				if stub == nil {
					callbackErr = errors.New("size skip node absent at the publication callback")
					return nil
				}
				observedKind = stub.Kind
				observedSkip = stub.Meta["skipped_due_to_size"]
				observedSize = stub.Meta["file_size_bytes"]
				observedLimit = stub.Meta["max_file_size_bytes"]
				if scenario == "cancel_after_capture" {
					cancel()
					return nil
				}
				// Preserve a valid GIF and its mtime while invalidating the
				// captured size version. The synchronous hook finishes before
				// IndexCtx can execute either receipt finalizer.
				data, err := os.ReadFile(gifPath)
				if err == nil {
					err = os.WriteFile(gifPath, append(data, make([]byte, 256)...), 0600)
				}
				if err == nil {
					err = os.Chtimes(gifPath, capturedInfo.ModTime(), capturedInfo.ModTime())
				}
				callbackErr = err
				return nil
			}))
			idx = newIndexer(logger)
			// The peer is durable-only at entry, so receipt publication must
			// not reconstruct an authoritative old roster from this map.
			idx.SetFileMtimes(map[string]int64{"demo.gif": oldMtime})
			result, indexErr := idx.IndexCtx(ctx, root)
			require.EqualValues(t, 1, callbackHits.Load(), "the source-verified publication seam must actually execute")
			require.NoError(t, callbackErr)
			require.Equal(t, graph.KindFile, observedKind)
			require.Equal(t, true, observedSkip)
			require.EqualValues(t, oldSize, observedSize, "the stub must describe the pre-callback version")
			require.EqualValues(t, limit, observedLimit)
			if scenario == "cancel_after_capture" {
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				// Some publication backends acknowledge this late cancellation
				// with an error, while others have already produced a result.
				// Both must withhold the cancelled attempt's receipt.
				if indexErr != nil {
					require.ErrorIs(t, indexErr, context.Canceled)
				} else {
					require.NotNil(t, result)
				}
			} else {
				require.NoError(t, indexErr)
				require.NotNil(t, result)
				currentInfo, err := os.Stat(gifPath)
				require.NoError(t, err)
				require.EqualValues(t, oldSize+256, currentInfo.Size())
				require.Equal(t, oldMtime, currentInfo.ModTime().UnixNano())
			}
			assert.NotContains(t, idx.publishFileMtimes(), "demo.gif", "the actual IndexCtx must invalidate the old local receipt")
			assert.NotContains(t, store.LoadFileMtimes("repo"), "demo.gif", "the actual IndexCtx must invalidate the old durable receipt")
			assert.Equal(t, peerMtime, store.LoadFileMtimes("repo")["durable-only.go"])
			idx.Close()
			idx = nil
			require.NoError(t, store.Close())
			store = nil
			store, err = store_sqlite.Open(dbPath)
			require.NoError(t, err)
			idx = newIndexer(zap.NewNop())
			idx.SetFileMtimes(store.LoadFileMtimes("repo"))
			idx.loadFileIndexFailures()
			assert.NotContains(t, store.LoadFileMtimes("repo"), "demo.gif")
			assert.Equal(t, peerMtime, store.LoadFileMtimes("repo")["durable-only.go"])
			changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
			require.NoError(t, err)
			assert.Equal(t, []string{"demo.gif"}, changed)
			require.Empty(t, deleted)
			require.Equal(t, 2, detected)
		})
	}
}
