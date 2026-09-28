package indexer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
)

func factoryChainPriorTree() map[string]string {
	return map[string]string{
		"pkg/a.go": `package pkg

type Builder struct{}

type Widget struct{}

func New() *Builder { return &Builder{} }

func (b *Builder) Build() *Widget { return &Widget{} }

func (w *Widget) Run() {}
`,
		"pkg/use.go": `package pkg

func Use() {
	New().Build().Run()
	New().Build().Missing()
	go func() { New().Build().Run() }()
}

func Other() {
	New().Build().Gone()
}
`,
	}
}

var factoryChainPriorEdits = map[string]string{
	// A line typed in a body: every chain moves, nothing is declared anew.
	"body_line": strings.Replace(factoryChainPriorTree()["pkg/use.go"],
		"func Use() {\n", "func Use() {\n\t_ = 1\n", 1),
	// The save declares the method a failed chain named: it must be walked.
	"declares_missing": strings.Replace(factoryChainPriorTree()["pkg/use.go"],
		"func Other() {", "func (w *Widget) Missing() {}\n\nfunc Other() {", 1),
}

func factoryChainCallRows(t *testing.T, store *store_sqlite.Store, file string) []string {
	t.Helper()
	var rows []string
	for edge := range store.EdgesByKind(graph.EdgeCalls) {
		if edge.FilePath != file {
			continue
		}
		via, _ := edge.Meta["via"].(string)
		meta, _ := json.Marshal(map[string]any{"via": via})
		rows = append(rows, fmt.Sprintf("%s|%s|%d|%s", edge.From, edge.To, edge.Line, meta))
	}
	sort.Strings(rows)
	return rows
}

// A per-save factory-chain pass does not walk again the chains that failed
// before a save that kept every declaration, and its rows are the whole
// index's either way.
func TestPerSaveFactoryChainSkipsOnlyChainsNothingMoved(t *testing.T) {
	builderIsolateGit(t)
	for name, edited := range factoryChainPriorEdits {
		t.Run(name, func(t *testing.T) {
			dir := builderTempDir(t, "fcp-"+name)
			builderWriteTree(t, dir, factoryChainPriorTree())
			store := builderOpenStore(t, "fcp-"+name)
			builderIndex(t, store, dir)
			idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
			defer idx.Close()
			idx.SetRepoPrefix(builderRepoPrefix)
			idx.SetWorkspaceID(builderRepoPrefix)
			idx.SetProjectID(builderRepoPrefix)
			idx.rootPath = dir
			path := filepath.Join(dir, "pkg", "use.go")
			if err := idx.IndexFile(path); err != nil {
				t.Fatalf("warm-up IndexFile: %v", err)
			}
			if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
				t.Fatal(err)
			}
			before := resolver.FactoryChainPriorSkipped()
			if err := idx.IndexFile(path); err != nil {
				t.Fatalf("IndexFile: %v", err)
			}
			skipped := resolver.FactoryChainPriorSkipped() - before
			switch name {
			case "body_line":
				if skipped == 0 {
					t.Fatal("a body-only save walked every failed chain again")
				}
			case "declares_missing":
				if skipped != 0 {
					t.Fatalf("a save that declared a chain's method skipped %d chains", skipped)
				}
			}
			file := builderRepoPrefix + "/pkg/use.go"
			perSave := factoryChainCallRows(t, store, "pkg/use.go")
			if len(perSave) == 0 {
				perSave = factoryChainCallRows(t, store, file)
			}
			cleanDir := builderTempDir(t, "fcp-clean-"+name)
			tree := factoryChainPriorTree()
			tree["pkg/use.go"] = edited
			builderWriteTree(t, cleanDir, tree)
			clean := builderOpenStore(t, "fcp-clean-"+name)
			builderIndex(t, clean, cleanDir)
			whole := factoryChainCallRows(t, clean, "pkg/use.go")
			if len(whole) == 0 {
				whole = factoryChainCallRows(t, clean, file)
			}
			if len(whole) == 0 {
				t.Fatal("fixture: the whole index recorded no call from use.go")
			}
			onlyPerSave, onlyWhole := diffRows(perSave, whole)
			if len(onlyPerSave) > 0 || len(onlyWhole) > 0 {
				t.Fatalf("call rows differ from a whole index of the edited tree\nper-save only:\n  %s\nwhole-index only:\n  %s",
					strings.Join(onlyPerSave, "\n  "), strings.Join(onlyWhole, "\n  "))
			}
		})
	}
}
