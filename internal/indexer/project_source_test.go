package indexer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// projectionFixtures holds one small source per language with the shapes a
// projection must reproduce: imports, constants, types, methods, calls and
// a TODO the coverage pass turns into a node.
var projectionFixtures = map[string]string{
	"go.mod": "module example.com/parity\n\ngo 1.22\n",
	"sample.go": `package parity

import (
	"fmt"
	"strings"
)

// TODO: tighten the limit.
const Limit = 3

type Greeter struct {
	Name string
}

func (g *Greeter) Greet(other string) string {
	return fmt.Sprintf("%s greets %s", g.Name, strings.ToUpper(other))
}

func helper(n int) int {
	if n > Limit {
		return n
	}
	return helper(n + 1)
}

var defaultGreeter = &Greeter{Name: "x"}
`,
	// The index strips a byte-order mark before parsing and hashing.
	"bom.go": "\xef\xbb\xbfpackage parity\n\nfunc Marked() int { return helper(1) }\n",
	"web/sample.ts": `import { readFileSync } from "fs";

export const LIMIT = 3;

export interface Shape {
  area(): number;
}

export class Square implements Shape {
  constructor(private side: number) {}
  area(): number {
    return this.side * this.side;
  }
}

export function load(path: string): string {
  // TODO: cache the result
  return readFileSync(path, "utf8");
}
`,
	"py/sample.py": `import os

LIMIT = 3


class Greeter:
    def __init__(self, name):
        self.name = name

    def greet(self, other):
        return helper(other)


def helper(value):
    # TODO: validate the value
    return os.path.join(value, "x")
`,
	"c/sample.c": `#include <stdio.h>

#define LIMIT 3

struct point {
    int x;
    int y;
};

static int helper(int n) {
    return n + LIMIT;
}

int main(void) {
    struct point p = {1, 2};
    printf("%d\n", helper(p.x));
    return 0;
}
`,
	"java/sample/Greeter.java": `package sample;

import java.util.List;

public class Greeter {
    private static final int LIMIT = 3;
    private final String name;

    public Greeter(String name) {
        this.name = name;
    }

    public String greet(List<String> others) {
        return name + others.size() + LIMIT;
    }
}
`,
	"rust/src/lib.rs": `use std::fmt;

pub const LIMIT: u32 = 3;

pub struct Greeter {
    name: String,
}

impl Greeter {
    pub fn greet(&self, other: &str) -> String {
        format!("{} {}", self.name, helper(other))
    }
}

fn helper(s: &str) -> String {
    s.to_uppercase()
}

impl fmt::Display for Greeter {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "{}", self.name)
    }
}
`,
}

func newProjectionTestIndexer(t *testing.T, g graph.Store, cfg config.IndexConfig) *Indexer {
	t.Helper()
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	cfg.Workers = 1
	idx := New(g, registry, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	return idx
}

func writeProjectionFixtures(t *testing.T, dir string) {
	t.Helper()
	for rel, content := range projectionFixtures {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

// The projection of a file equals what a full index stores for it, node for
// node, and carries the same content hash as the file's metadata row. Every
// language on the allowlist must hold this; the rest must refuse.
func TestProjectSourceGoldenParity(t *testing.T) {
	dir := t.TempDir()
	writeProjectionFixtures(t, dir)
	g := graph.New()
	idx := newProjectionTestIndexer(t, g, config.Default().Index)
	idx.SetRepoPrefix("parity")
	idx.SetWorkspaceID("ws")
	idx.SetProjectID("proj")
	_, err := idx.Index(dir)
	require.NoError(t, err)

	for rel := range projectionFixtures {
		if rel == "go.mod" {
			continue
		}
		t.Run(rel, func(t *testing.T) {
			abs := filepath.Join(dir, filepath.FromSlash(rel))
			src, err := os.ReadFile(abs)
			require.NoError(t, err)
			lang, ok := idx.effectiveLanguage(abs, src)
			require.True(t, ok)

			projection, err := idx.ProjectSource(context.Background(), abs, src)
			if !projectionLanguages[lang] {
				require.ErrorIs(t, err, ErrProjectionLanguage)
				return
			}
			require.NoError(t, err)
			graphPath := "parity/" + rel
			require.Equal(t, graphPath, projection.GraphPath)
			require.NotEmpty(t, projection.Nodes)
			require.True(t, projection.ParseChecked)
			require.Zero(t, projection.ParseErrors)
			require.Empty(t, DiffProjectedNodes(projection.Nodes, g.GetFileNodes(graphPath)))
			for _, n := range projection.Nodes {
				require.Equal(t, "parity", n.RepoPrefix, n.ID)
				require.Equal(t, "ws", n.WorkspaceID, n.ID)
				require.Equal(t, "proj", n.ProjectID, n.ID)
			}

			rows, err := g.FileMetasByPaths("parity", []string{graphPath})
			require.NoError(t, err)
			require.Equal(t, rows[graphPath].ContentHash, projection.ContentHash)
		})
	}
}

type slowGoExtractor struct{ delay time.Duration }

func (slowGoExtractor) Language() string     { return "go" }
func (slowGoExtractor) Extensions() []string { return []string{".go"} }
func (e slowGoExtractor) Extract(path string, _ []byte) (*parser.ExtractionResult, error) {
	time.Sleep(e.delay)
	return &parser.ExtractionResult{Nodes: []*graph.Node{{ID: path, Kind: graph.KindFile, FilePath: path}}}, nil
}

func TestProjectSourceRefusals(t *testing.T) {
	dir := t.TempDir()
	goSource := []byte("package p\n\nfunc F() {}\n")

	t.Run("language off the allowlist", func(t *testing.T) {
		idx := newProjectionTestIndexer(t, graph.New(), config.Default().Index)
		idx.SetRootPath(dir)
		_, err := idx.ProjectSource(t.Context(), filepath.Join(dir, "x.rb"), []byte("def f\nend\n"))
		require.ErrorIs(t, err, ErrProjectionLanguage)
	})
	t.Run("over 1 MiB", func(t *testing.T) {
		idx := newProjectionTestIndexer(t, graph.New(), config.Default().Index)
		idx.SetRootPath(dir)
		big := append(bytes.Clone(goSource), bytes.Repeat([]byte("// padding\n"), (1<<20)/11+1)...)
		_, err := idx.ProjectSource(t.Context(), filepath.Join(dir, "big.go"), big)
		require.ErrorIs(t, err, ErrProjectionTooLarge)
	})
	t.Run("command transform", func(t *testing.T) {
		cfg := config.Default().Index
		cfg.Transforms = []config.TransformRule{{
			Name:       "must-not-run",
			Extensions: []string{".go"},
			Command:    []string{"gortex-test-transform-must-not-run"},
		}}
		idx := newProjectionTestIndexer(t, graph.New(), cfg)
		idx.SetRootPath(dir)
		_, err := idx.ProjectSource(t.Context(), filepath.Join(dir, "f.go"), goSource)
		require.ErrorIs(t, err, ErrProjectionTransform)
	})
	t.Run("build past the deadline", func(t *testing.T) {
		const parse = 1500 * time.Millisecond
		registry := parser.NewRegistry()
		registry.Register(slowGoExtractor{delay: parse})
		idx := New(graph.New(), registry, config.Default().Index, zap.NewNop())
		t.Cleanup(idx.Close)
		idx.SetRootPath(dir)
		started := time.Now()
		_, err := idx.ProjectSource(t.Context(), filepath.Join(dir, "f.go"), goSource)
		require.ErrorIs(t, err, ErrProjectionDeadline)
		// The caller hears back at the deadline, not when the parse ends.
		require.Less(t, time.Since(started), parse/2)
	})
	t.Run("no parse slot", func(t *testing.T) {
		idx := newProjectionTestIndexer(t, graph.New(), config.Default().Index)
		idx.SetRootPath(dir)
		held, err := acquireParseAdmission(t.Context(), projectionParseAdmission.capacity, 0, nil, projectionParseAdmission)
		require.NoError(t, err)
		_, err = idx.ProjectSource(t.Context(), filepath.Join(dir, "f.go"), goSource)
		held.Release()
		require.ErrorIs(t, err, ErrProjectionNoSlot)
	})
}

// Bulk indexing can fill the shared parse budget for minutes. A projection
// build has its own reserve and must not queue behind it.
func TestInteractiveExtractionNotBlockedByFullSharedParseAdmission(t *testing.T) {
	dir := t.TempDir()
	writeProjectionFixtures(t, dir)
	idx := newProjectionTestIndexer(t, graph.New(), config.Default().Index)
	idx.SetRootPath(dir)
	raw, native := newParseAdmissionBudget(1<<20), newParseAdmissionBudget(1<<20)
	idx.parseAdmission.Store(raw)
	idx.nativeParseAdmission.Store(native)
	for _, budget := range []*parseAdmissionBudget{raw, native} {
		held, err := acquireParseAdmission(t.Context(), budget.capacity, 0, nil, budget)
		require.NoError(t, err)
		t.Cleanup(held.Release)
	}

	for _, rel := range []string{"sample.go", "c/sample.c"} {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		src, err := os.ReadFile(abs)
		require.NoError(t, err)
		projection, err := idx.ProjectSource(t.Context(), abs, src)
		require.NoError(t, err, rel)
		require.True(t, strings.HasSuffix(projection.GraphPath, rel))
		require.NotEmpty(t, projection.Nodes)
	}
}

func TestProjectSourceUnknownVerdictCannotCertifyClean(t *testing.T) {
	idx := newProjectionTestIndexer(t, graph.New(), config.Default().Index)
	root := t.TempDir()
	idx.SetRootPath(root)
	idx.registry.Register(slowGoExtractor{})
	projection, err := idx.ProjectSource(context.Background(), filepath.Join(root, "main.go"), []byte("package p\n"))
	require.NoError(t, err)
	require.False(t, projection.ParseChecked)
	require.Zero(t, projection.ParseErrors)
}
