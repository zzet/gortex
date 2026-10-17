package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The edge kinds no pass is known to restate after an eviction, each held by
// one file INTO a file an edit re-derives: a test function's tests edge into
// the function it calls, a type alias's aliases edge into its underlying
// type, a goroutine launch's spawns edge into the launched function, a JSX
// parent's renders_child edge into the child component, a NATS subscriber's
// listens_on into the topic node a publisher's file also declares, and a
// decorator's annotated edge. Each target file is then edited (a body edit
// that keeps every identity), and the composed view is compared by kind with
// a clean index through the kind comparison (kindParityCheck).
//
// covered_by is not here: it comes from an imported coverage profile, not
// from an index, so no edit re-derives it.
func kindParityRestateTree() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/restate\n\ngo 1.24\n",
		"prod/target.go": "package prod\n\n// Base is aliased from another file.\ntype Base struct{ n int }\n\n" +
			"// Target is called by a test in another file.\nfunc Target() int {\n\treturn 1\n}\n\n" +
			"// Worker is launched by a goroutine in another file.\nfunc Worker() {\n}\n",
		"prod/target_test.go": "package prod\n\nimport \"testing\"\n\nfunc TestTarget(t *testing.T) {\n\tif Target() != 1 {\n\t\tt.Fatal(\"target\")\n\t}\n}\n",
		"prod/alias.go":       "package prod\n\n// Alias names Base.\ntype Alias = Base\n",
		"prod/spawner.go":     "package prod\n\n// Spawn launches Worker.\nfunc Spawn() {\n\tgo Worker()\n}\n",
		"web/Child.tsx":       "export function Child() {\n  return <div>child</div>;\n}\n",
		"web/Parent.tsx":      "import { Child } from './Child';\n\nexport function Parent() {\n  return <Child />;\n}\n",
		"events/pub.go":       "package events\n\nimport \"github.com/nats-io/nats.go\"\n\n// Send publishes orders.created.\nfunc Send(nc *nats.Conn) {\n\tnc.Publish(\"orders.created\", []byte(\"x\"))\n}\n",
		"events/sub.go":       "package events\n\nimport \"github.com/nats-io/nats.go\"\n\nfunc handler(m *nats.Msg) {}\n\n// Listen subscribes to orders.created.\nfunc Listen(nc *nats.Conn) {\n\tnc.Subscribe(\"orders.created\", handler)\n}\n",
		"py/deco.py":          "def traced(fn):\n    return fn\n",
		"py/use.py":           "from deco import traced\n\n\n@traced\ndef handler():\n    return 1\n",
	}
}

// bodyEdit appends a statement to the file's first body without moving any
// identity: a comment line before the first closing brace.
func kindParityRestateEdits() map[string]string {
	return map[string]string{
		"prod/target.go": strings.Replace(kindParityRestateTree()["prod/target.go"], "\treturn 1\n", "\t_ = 0\n\treturn 1\n", 1),
		"web/Child.tsx":  "export function Child() {\n  const label = 'child';\n  return <div>{label}</div>;\n}\n",
		"events/pub.go":  strings.Replace(kindParityRestateTree()["events/pub.go"], "\tnc.Publish(", "\t_ = 0\n\tnc.Publish(", 1),
		"py/deco.py":     "def traced(fn):\n    wrapped = fn\n    return wrapped\n",
	}
}

func TestKindParityEdgeKindsWithoutARestater(t *testing.T) {
	for path, content := range kindParityRestateEdits() {
		t.Run(strings.ReplaceAll(path, "/", "_"), func(t *testing.T) {
			builderIsolateGit(t)
			name := "kind-restate-" + strings.NewReplacer("/", "-", ".", "-").Replace(path)
			store := builderOpenStore(t, name)
			repoDir := builderTempDir(t, "checkout-"+name)
			builderGit(t, repoDir, "init", "--initial-branch=main")
			builderWriteTree(t, repoDir, kindParityRestateTree())
			builderGit(t, repoDir, "add", "-A")
			builderGit(t, repoDir, "commit", "-q", "-m", "base")
			builderIndex(t, store, repoDir)
			builder := builderNewBuilder(store)
			h := newDirtyChainBuilder(t, builder, store, repoDir, false)
			builderWriteFile(t, repoDir, path, content)
			id, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
			if err != nil {
				t.Fatal(err)
			}
			kindParityCheck(t, "restate/"+path, repoDir, dirtyChainComposed(t, store, []int64{id}), generationCovered(t, store, id), []string{path})
		})
	}
}

// covered_by comes from an imported coverage profile, recorded at the covered
// function's own file, so a delta re-deriving that file re-derives none of
// it. It survives an edit that keeps the function's body, and goes with a
// changed body (the next import measures it again).
func TestCoveredByFollowsTheCoveredBodyAcrossAnEdit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edit   string
		expect bool
	}{
		{"unchanged body", strings.Replace(kindParityRestateTree()["prod/target.go"], "func Worker() {\n}\n", "func Worker() {\n\t_ = 1\n}\n", 1), true},
		{"changed body", kindParityRestateEdits()["prod/target.go"], false},
	} {
		t.Run(strings.ReplaceAll(tc.name, " ", "_"), func(t *testing.T) {
			builderIsolateGit(t)
			name := "covered-by-" + strings.ReplaceAll(tc.name, " ", "-")
			store := builderOpenStore(t, name)
			repoDir := builderTempDir(t, "checkout-"+name)
			builderGit(t, repoDir, "init", "--initial-branch=main")
			builderWriteTree(t, repoDir, kindParityRestateTree())
			builderGit(t, repoDir, "add", "-A")
			builderGit(t, repoDir, "commit", "-q", "-m", "base")
			builderIndex(t, store, repoDir)
			from, to := builderRepoPrefix+"/prod/target.go::Target", builderRepoPrefix+"/prod/target_test.go::TestTarget"
			store.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeCoveredBy, FilePath: builderRepoPrefix + "/prod/target.go", Line: 8, Origin: graph.OriginASTInferred})
			builder := builderNewBuilder(store)
			h := newDirtyChainBuilder(t, builder, store, repoDir, false)
			builderWriteFile(t, repoDir, "prod/target.go", tc.edit)
			id, _, err := builder.BuildDirtyLayer(context.Background(), h.request())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range dirtyChainComposed(t, store, []int64{id}).GetOutEdges(from) {
				if e.Kind == graph.EdgeCoveredBy && e.To == to {
					found = true
				}
			}
			if found != tc.expect {
				t.Fatalf("covered_by after the edit: present=%t, want %t", found, tc.expect)
			}
		})
	}
}
