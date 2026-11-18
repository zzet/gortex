package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func rustImplementsEdges(t *testing.T, src string) map[string]*graph.Edge {
	t.Helper()
	_, edges := rustNodesByID(t, src)
	out := map[string]*graph.Edge{}
	for _, e := range edges {
		if e.Kind == graph.EdgeImplements {
			out[e.From+" -> "+e.To] = e
		}
	}
	return out
}

// A derive really does implement the trait — it is how most Rust types
// acquire Debug, Clone and Serialize. Only an annotation edge was
// emitted, leaving find_implementations blind to every one of them.
func TestRsExtractor_DeriveImplementsTrait(t *testing.T) {
	impls := rustImplementsEdges(t, `#[derive(Debug, serde::Serialize)]
pub struct Dto { x: i32 }
`)
	require.Contains(t, impls, "probe.rs::Dto -> unresolved::Debug")
	assert.Equal(t, "derive", impls["probe.rs::Dto -> unresolved::Debug"].Meta["via"])
	require.Contains(t, impls, "probe.rs::Dto -> unresolved::Serialize",
		"a path-qualified derive resolves to its base trait name")
}

// A marker impl carries no methods, so the method-level EdgeOverrides
// that used to be the only signal left no trace of it at all.
func TestRsExtractor_ImplBlockImplementsTrait(t *testing.T) {
	impls := rustImplementsEdges(t, `struct Handle;
impl Send for Handle {}
impl std::fmt::Display for Handle { fn fmt(&self) {} }
`)
	require.Contains(t, impls, "probe.rs::Handle -> unresolved::Send",
		"a marker impl with no methods must still record the relationship")
	assert.Equal(t, "impl", impls["probe.rs::Handle -> unresolved::Send"].Meta["via"])
	require.Contains(t, impls, "probe.rs::Handle -> unresolved::Display",
		"a fully-qualified trait path reduces to its base name")
}

// Attributes written on an impl block were dropped wholesale, which is
// why the file-level wasm_bindgen sentinel only ever saw function-level
// attributes.
func TestRsExtractor_ImplBlockAttributes(t *testing.T) {
	e := NewRustExtractor()
	result, err := e.Extract("probe.rs", []byte(`struct Dto;
#[wasm_bindgen]
impl Dto { pub fn go(&self) {} }
`))
	require.NoError(t, err)

	var annotated bool
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeAnnotated && ed.From == "probe.rs::Dto" &&
			ed.To == "annotation::rust::wasm_bindgen" {
			annotated = true
		}
	}
	assert.True(t, annotated, "an impl-block attribute attaches to the impl target")

	for _, n := range result.Nodes {
		if n.Kind == graph.KindFile {
			assert.Equal(t, true, n.Meta["uses_wasm_bindgen"],
				"the interop sentinel must fire for an impl-level #[wasm_bindgen]")
		}
	}
}

// An impl for a type this file does not declare (another file, another
// crate, a primitive) has no node to source the implements edge at.
// Emitting <file>::<Type> anyway left edges no node owns, which outlived
// their repository on eviction (#1120).
func TestRsExtractor_ImplForForeignTypeEmitsNoDanglingEdge(t *testing.T) {
	e := NewRustExtractor()
	result, err := e.Extract("probe.rs", []byte(`use other::Strictness;
impl From<u32> for Strictness { fn from(_: u32) -> Self { todo!() } }
impl MyExt for u8 {}
#[wasm_bindgen]
impl Strictness { pub fn go(&self) {} }
struct Local;
impl Send for Local {}
`))
	require.NoError(t, err)

	nodes := make(map[string]bool, len(result.Nodes))
	for _, n := range result.Nodes {
		nodes[n.ID] = true
	}
	for _, ed := range result.Edges {
		if ed.Kind != graph.EdgeImplements && ed.Kind != graph.EdgeAnnotated {
			continue
		}
		assert.True(t, nodes[ed.From], "%s edge %s -> %s has no source node", ed.Kind, ed.From, ed.To)
	}

	impls := rustImplementsEdges(t, `use other::Strictness;
impl From<u32> for Strictness {}
impl MyExt for u8 {}
struct Local;
impl Send for Local {}
`)
	assert.NotContains(t, impls, "probe.rs::Strictness -> unresolved::From")
	assert.NotContains(t, impls, "probe.rs::u8 -> unresolved::MyExt")
	require.Contains(t, impls, "probe.rs::Local -> unresolved::Send",
		"an impl for a type declared in the file keeps its edge")

	for _, n := range result.Nodes {
		if n.Kind == graph.KindFile {
			assert.Equal(t, true, n.Meta["uses_wasm_bindgen"],
				"the interop sentinel still fires for #[wasm_bindgen] on an impl of a foreign type")
		}
	}
}

// The type may be declared after the impl that names it; the edge must
// survive regardless of order.
func TestRsExtractor_ImplBeforeTypeDeclarationKeepsEdge(t *testing.T) {
	impls := rustImplementsEdges(t, `impl Send for Later {}
struct Later;
`)
	require.Contains(t, impls, "probe.rs::Later -> unresolved::Send")
}
