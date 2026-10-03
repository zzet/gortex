package parser

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/golang"
	"github.com/zzet/gortex/internal/parser/tsitter/python"
)

func TestParseFile_Go(t *testing.T) {
	src := []byte(`package main

func Hello() {
	fmt.Println("hello")
}
`)
	tree, err := ParseFile(src, golang.GetLanguage())
	require.NoError(t, err)
	defer tree.Close()

	root := tree.RootNode()
	assert.Equal(t, "source_file", root.Type())
	assert.True(t, root.ChildCount() > 0)
}

func TestRunQuery_GoFunction(t *testing.T) {
	src := []byte(`package main

func Hello() {}
func World(x int) string { return "" }
`)
	tree, err := ParseFile(src, golang.GetLanguage())
	require.NoError(t, err)
	defer tree.Close()

	pattern := `(function_declaration name: (identifier) @func.name) @func.def`
	results, err := RunQuery(pattern, golang.GetLanguage(), tree.RootNode(), src)
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t, "Hello", results[0].Captures["func.name"].Text)
	assert.Equal(t, "World", results[1].Captures["func.name"].Text)
}

func TestRunQuery_NoMatches(t *testing.T) {
	src := []byte(`package main

var x = 42
`)
	tree, err := ParseFile(src, golang.GetLanguage())
	require.NoError(t, err)
	defer tree.Close()

	pattern := `(function_declaration name: (identifier) @func.name) @func.def`
	results, err := RunQuery(pattern, golang.GetLanguage(), tree.RootNode(), src)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestParseFile_InvalidSource(t *testing.T) {
	// tree-sitter is error-tolerant; it returns a tree even for garbage input.
	src := []byte(`{{{{{not valid go at all!!!!`)
	tree, err := ParseFile(src, golang.GetLanguage())
	require.NoError(t, err)
	defer tree.Close()

	root := tree.RootNode()
	assert.NotNil(t, root) // just verify it doesn't crash
}

// TestParseFile_PoolGrammarSwitch verifies that a pooled parser, after
// parsing one grammar, correctly re-binds to a different grammar on the
// next checkout — the exact failure mode the legacy smacker pool hit.
func TestParseFile_PoolGrammarSwitch(t *testing.T) {
	goSrc := []byte("package main\n\nfunc Hello() {}\n")
	pySrc := []byte("def hello():\n    return 1\n")

	for i := 0; i < 20; i++ {
		goTree, err := ParseFile(goSrc, golang.GetLanguage())
		require.NoError(t, err)
		assert.Equal(t, "source_file", goTree.RootNode().Type())
		assert.False(t, goTree.RootNode().HasError())
		goTree.Close()

		pyTree, err := ParseFile(pySrc, python.GetLanguage())
		require.NoError(t, err)
		assert.Equal(t, "module", pyTree.RootNode().Type())
		assert.False(t, pyTree.RootNode().HasError())
		pyTree.Close()
	}
}

// TestParseFile_PoolConcurrent exercises the parser pool from many
// goroutines at once. Run under -race, it proves checkout/return is
// data-race free and that concurrent callers never share a live parser.
func TestParseFile_PoolConcurrent(t *testing.T) {
	srcs := [][]byte{
		[]byte("package a\n\nfunc A() int { return 1 }\n"),
		[]byte("package b\n\nfunc B() string { return \"\" }\n"),
		[]byte("package c\n\ntype C struct{ X int }\n"),
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			src := srcs[n%len(srcs)]
			tree, err := ParseFile(src, golang.GetLanguage())
			if err != nil {
				t.Errorf("concurrent parse %d: %v", n, err)
				return
			}
			defer tree.Close()
			if tree.RootNode().Type() != "source_file" {
				t.Errorf("concurrent parse %d: unexpected root %q", n, tree.RootNode().Type())
			}
		}(i)
	}
	wg.Wait()
}

func TestParseFile_RejectsUTF16Source(t *testing.T) {
	body := []byte("package main\n")
	for name, bom := range map[string][]byte{
		"utf16le": {0xFF, 0xFE},
		"utf16be": {0xFE, 0xFF},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			tree, err := ParseFile(append(bom, body...), golang.GetLanguage())
			elapsed := time.Since(start)
			require.ErrorIs(t, err, ErrUTF16Source)
			assert.Nil(t, tree)
			assert.Less(t, elapsed, time.Second,
				"the UTF-16 guard must fail fast, before any parse work")
		})
	}
	t.Run("utf8-still-parses", func(t *testing.T) {
		tree, err := ParseFile(append([]byte{0xEF, 0xBB, 0xBF}, body...), golang.GetLanguage())
		require.NoError(t, err)
		require.NotNil(t, tree)
		tree.Close()
	})
}

// TestParseFile_RejectsBinarySource pins the binary-content guard: a
// NUL byte in the sniff window means the bytes are not text any grammar
// can consume (a tool-cache .pkl claimed by the Pkl extension was the
// reported case), and the guard must fail fast — before the parse pool
// or any error-recovery balancing work.
func TestParseFile_RejectsBinarySource(t *testing.T) {
	pickle := append([]byte("\x80\x04\x95\x1a\x00\x00"), make([]byte, 512)...)
	start := time.Now()
	tree, err := ParseFile(pickle, golang.GetLanguage())
	elapsed := time.Since(start)
	require.ErrorIs(t, err, ErrBinarySource)
	assert.Nil(t, tree)
	assert.Less(t, elapsed, time.Second,
		"the binary guard must fail fast, before any parse work")

	t.Run("nul-beyond-window-is-not-sniffed", func(t *testing.T) {
		// The sniff covers the first 8 KiB only. Text whose first NUL
		// sits past the window parses (tree-sitter is error-tolerant);
		// the indexer's own sniff of the full prefix is what catches it.
		late := append([]byte(strings.Repeat("a", binarySniffBytes)), 0x00)
		tree, err := ParseFile(late, golang.GetLanguage())
		if err == nil {
			tree.Close()
		}
		require.NoError(t, err)
	})

	t.Run("text-still-parses", func(t *testing.T) {
		tree, err := ParseFile([]byte("package main\n\nfunc A() {}\n"), golang.GetLanguage())
		require.NoError(t, err)
		tree.Close()
	})
}

// TestParseCtx_DeadlineAbortsPathologicalParse pins existing behaviour:
// NUL-interleaved (UTF-16-shaped) bytes drive tree-sitter's error-recovery
// balancing hard, and the context deadline IS honored in every phase —
// tree-sitter's progress check fires in balancing too. The parse must
// abort at the budget.
func TestParseCtx_DeadlineAbortsPathologicalParse(t *testing.T) {
	var b bytes.Buffer
	pattern := []byte("f\x00u\x00n\x00c\x00 \x00x\x00(\x00)\x00 \x00{\x00 \x00a\x00=\x00b\x00;\x00 \x00}\x00\n\x00")
	for b.Len() < 1<<20 {
		b.Write(pattern)
	}
	src := b.Bytes()

	p := tsitter.NewParser()
	defer p.Close()
	p.SetLanguage(golang.GetLanguage())

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := p.ParseCtx(ctx, nil, src)
	elapsed := time.Since(start)
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the pathological parse must exceed the budget and abort, not finish in %v", elapsed)
	assert.Less(t, elapsed, 5*time.Second,
		"the deadline must abort the parse, not merely be recorded")
}
