package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

const contentFinalizeRepo = "content-finalize-repo"

type cancelOnContentOpenSource struct {
	source.ContentSource
	path   string
	cancel context.CancelFunc
}

func (s *cancelOnContentOpenSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	if path == s.path {
		s.cancel()
		return nil, source.FileMeta{}, context.Canceled
	}
	return s.ContentSource.Open(path)
}

func seedContentFinalizeRow(
	t testing.TB,
	store *store_sqlite.Store,
	filePath, token string,
) {
	t.Helper()
	if err := store.AppendContent(contentFinalizeRepo, []graph.ContentFTSItem{{
		NodeID: filePath + "::section:0", FilePath: filePath, Body: token,
	}}); err != nil {
		t.Fatalf("seed content row %q: %v", filePath, err)
	}
	if err := store.BuildContentIndex(); err != nil {
		t.Fatalf("finalize seeded content row %q: %v", filePath, err)
	}
}

func requireContentFinalizeHit(
	t testing.TB,
	store *store_sqlite.Store,
	token string,
	want bool,
) {
	t.Helper()
	hits, err := store.SearchContent(token, contentFinalizeRepo, 8)
	if err != nil {
		t.Fatalf("search content %q: %v", token, err)
	}
	if got := len(hits) > 0; got != want {
		t.Fatalf("content hit for %q = %t, want %t: %+v", token, got, want, hits)
	}
}

func writeContentFinalizeTree(t testing.TB, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("create content source directory: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("write content source file: %v", err)
		}
	}
	return root
}

func contentFinalizeShadowDecision(t testing.TB, logs *observer.ObservedLogs) bool {
	t.Helper()
	entries := logs.FilterMessage("indexer: shadow-swap decision").All()
	if len(entries) != 1 {
		t.Fatalf("shadow decisions = %d, want 1", len(entries))
	}
	taken, ok := entries[0].ContextMap()["shadow_taken"].(bool)
	if !ok {
		t.Fatalf("shadow decision has no boolean shadow_taken: %#v", entries[0].ContextMap())
	}
	return taken
}

func runContentFinalizeIndex(
	t *testing.T,
	target *store_sqlite.Store,
	root string,
	content source.ContentSource,
	shadow bool,
	ctx context.Context,
) error {
	t.Helper()
	if shadow {
		t.Setenv("GORTEX_SHADOW_MAX_FILES", "1000000")
		t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
	} else {
		t.Setenv("GORTEX_SHADOW_MAX_FILES", "0")
	}
	core, logs := observer.New(zapcore.InfoLevel)
	idx := newContentSourceIndexer(t, target)
	idx.config.Workers = 1
	idx.logger = zap.New(core)
	idx.SetRepoPrefix(contentFinalizeRepo)
	idx.SetWorkspaceID(contentFinalizeRepo)
	idx.SetProjectID(contentFinalizeRepo)
	idx.SetContentSource(content)
	defer idx.Close()
	_, err := idx.IndexCtx(ctx, root)
	if taken := contentFinalizeShadowDecision(t, logs); taken != shadow {
		t.Fatalf("shadow_taken = %t, want %t", taken, shadow)
	}
	return err
}

func TestAuthoritativeContentSourceFinalizeSweepsOnlyTargetGeneration(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(fmt.Sprintf("shadow_%t", shadow), func(t *testing.T) {
			store := builderOpenStore(t, fmt.Sprintf("content-finalize-%t", shadow))
			target := store.AtGeneration(41)
			sibling := store.AtGeneration(42)
			seedContentFinalizeRow(t, store, contentFinalizeRepo+"/base.txt", "basecontenttoken")
			seedContentFinalizeRow(t, sibling, contentFinalizeRepo+"/sibling.txt", "siblingcontenttoken")
			seedContentFinalizeRow(t, target, contentFinalizeRepo+"/stale.txt", "staletargetcontent")

			root := writeContentFinalizeTree(t, map[string]string{
				"fresh.txt": "freshgenerationcontent " + strings.Repeat("body ", 80),
			})
			content, err := source.NewFilesystemSource(root)
			if err != nil {
				t.Fatalf("open content source: %v", err)
			}
			defer content.Close() //nolint:errcheck // read-only test source
			if err := runContentFinalizeIndex(t, target, root, content, shadow, context.Background()); err != nil {
				t.Fatalf("index authoritative zero-mtime content source: %v", err)
			}

			requireContentFinalizeHit(t, target, "staletargetcontent", false)
			requireContentFinalizeHit(t, target, "freshgenerationcontent", true)
			requireContentFinalizeHit(t, store, "basecontenttoken", true)
			requireContentFinalizeHit(t, sibling, "siblingcontenttoken", true)
		})
	}
}

func TestInterruptedContentSourceIndexRetainsUnvisitedContent(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(fmt.Sprintf("shadow_%t", shadow), func(t *testing.T) {
			store := builderOpenStore(t, fmt.Sprintf("content-interrupted-%t", shadow))
			target := store.AtGeneration(51)
			sibling := store.AtGeneration(52)
			seedContentFinalizeRow(t, store, contentFinalizeRepo+"/base.txt", "interruptedbasecontent")
			seedContentFinalizeRow(t, sibling, contentFinalizeRepo+"/sibling.txt", "interruptedsiblingcontent")
			seedContentFinalizeRow(t, target, contentFinalizeRepo+"/unvisited.txt", "unvisitedstalecontent")

			root := writeContentFinalizeTree(t, map[string]string{
				"a.txt": "partialfreshcontent " + strings.Repeat("body ", 100),
				"z.txt": "cancel this read",
			})
			inner, err := source.NewFilesystemSource(root)
			if err != nil {
				t.Fatalf("open interrupted source: %v", err)
			}
			defer inner.Close() //nolint:errcheck // read-only test source
			ctx, cancel := context.WithCancel(context.Background())
			content := &cancelOnContentOpenSource{
				ContentSource: inner, path: "z.txt", cancel: cancel,
			}
			err = runContentFinalizeIndex(t, target, root, content, shadow, ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted index error = %v, want context cancellation", err)
			}

			requireContentFinalizeHit(t, target, "unvisitedstalecontent", true)
			requireContentFinalizeHit(t, target, "partialfreshcontent", true)
			requireContentFinalizeHit(t, store, "interruptedbasecontent", true)
			requireContentFinalizeHit(t, sibling, "interruptedsiblingcontent", true)
		})
	}
}

func BenchmarkAuthoritativeContentFinalizeSweep(b *testing.B) {
	const rows = 10_000
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		store, err := store_sqlite.Open(filepath.Join(
			b.TempDir(), fmt.Sprintf("content-finalize-%d.sqlite", i)))
		if err != nil {
			b.Fatalf("open benchmark store: %v", err)
		}
		target := store.AtGeneration(61)
		items := make([]graph.ContentFTSItem, rows)
		keep := make(map[string]struct{}, rows/2)
		for row := range rows {
			path := fmt.Sprintf("%s/doc-%05d.txt", contentFinalizeRepo, row)
			items[row] = graph.ContentFTSItem{
				NodeID: path + "::section:0", FilePath: path,
				Body: fmt.Sprintf("benchmarkcontenttoken%d", row),
			}
			if row%2 == 0 {
				keep[path] = struct{}{}
			}
		}
		if err := target.AppendContent(contentFinalizeRepo, items); err != nil {
			_ = store.Close()
			b.Fatalf("seed benchmark content: %v", err)
		}
		sweeper, ok := any(target).(interface {
			DeleteContentFilesForRepoNotIn(string, map[string]struct{}) error
		})
		if !ok {
			_ = store.Close()
			b.Fatal("SQLite store lacks authoritative content sweep")
		}

		b.StartTimer()
		err = sweeper.DeleteContentFilesForRepoNotIn(contentFinalizeRepo, keep)
		b.StopTimer()
		if err != nil {
			_ = store.Close()
			b.Fatalf("finalize benchmark content: %v", err)
		}
		if err := store.Close(); err != nil {
			b.Fatalf("close benchmark store: %v", err)
		}
	}
	b.ReportMetric(rows, "rows/op")
	b.ReportMetric(rows/2, "kept_files/op")
}
