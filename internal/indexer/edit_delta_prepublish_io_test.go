package indexer

import (
	"context"
	"strings"
	"testing"
)

// The build-phases line's prepublish_store_io is measured on the edit delta
// path as on the sparse builder's: an earlier build line carried null for it.
func TestEditDeltaReportsPrepublishIO(t *testing.T) {
	builderIsolateGit(t)
	store := builderOpenStore(t, "prepublish-io")
	repoDir := builderTempDir(t, "checkout-prepublish-io")
	src := "package store\n\nfunc Count(path string) int {\n\treturn len(path)\n}\n"
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, map[string]string{"go.mod": "module example.com/s\n\ngo 1.22\n", "store/store.go": src})
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, false)
	builderWriteFile(t, repoDir, "store/store.go", strings.Replace(src, "return len(path)", "n := len(path)\n\treturn n", 1))
	req := h.request()
	req.Base = commitLayerBase{Reader: store, corpus: store, stack: []int64{int64(1) << 40}}
	recordLastEditDelta(nil)
	_, report, err := builder.BuildDirtyLayer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if LastEditDeltaReport() == nil {
		t.Fatal("the edit was not built as a delta")
	}
	if report.PrepublishIO == nil {
		t.Fatal("the edit delta's report has no prepublish_store_io")
	}
	for _, key := range []string{"cpu_ms", "major_faults", "writer_main_reads", "mapped_pages"} {
		if _, ok := report.PrepublishIO[key]; !ok {
			t.Errorf("prepublish_store_io lacks %s: %v", key, report.PrepublishIO)
		}
	}
}
