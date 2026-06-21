package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// typedAsEdgeTo reports whether edges contains an EdgeTypedAs to
// unresolved::<name> carrying the given Meta["use_kind"]. A blank
// useKind matches any.
func typedAsEdgeTo(edges []*graph.Edge, name, useKind string) bool {
	target := "unresolved::" + name
	for _, e := range edges {
		if e.Kind != graph.EdgeTypedAs || e.To != target {
			continue
		}
		if useKind == "" {
			return true
		}
		if uk, _ := e.Meta["use_kind"].(string); uk == useKind {
			return true
		}
	}
	return false
}

func TestTSCast_AsSimpleType(t *testing.T) {
	src := `function f(x: unknown) {
	const el = x as ExcalidrawElement;
	return el;
}
`
	_, edges := runTSExtract(t, "src/cast.ts", src)
	if !typedAsEdgeTo(edges, "ExcalidrawElement", "cast") {
		t.Errorf("expected EdgeTypedAs use_kind=cast → unresolved::ExcalidrawElement; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
}

func TestTSCast_AsArrayType(t *testing.T) {
	src := `function f(x: unknown) {
	const els = x as ExcalidrawElement[];
	return els;
}
`
	_, edges := runTSExtract(t, "src/cast.ts", src)
	if !typedAsEdgeTo(edges, "ExcalidrawElement", "cast") {
		t.Errorf("expected EdgeTypedAs use_kind=cast → unresolved::ExcalidrawElement from array cast; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
}

func TestTSCast_AsGenericType(t *testing.T) {
	src := `function f(x: unknown) {
	const el = x as NonDeleted<ExcalidrawElement>;
	return el;
}
`
	_, edges := runTSExtract(t, "src/cast.ts", src)
	// The inner user type must surface; NonDeleted is a user-defined
	// utility generic so it surfaces too (not a builtin container).
	if !typedAsEdgeTo(edges, "ExcalidrawElement", "cast") {
		t.Errorf("expected EdgeTypedAs use_kind=cast → unresolved::ExcalidrawElement from generic cast; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
}

func TestTSCast_AsPrimitiveEmitsNone(t *testing.T) {
	src := `function f(x: unknown) {
	const n = x as number;
	return n;
}
`
	_, edges := runTSExtract(t, "src/cast.ts", src)
	for _, e := range edgesByKind(edges, graph.EdgeTypedAs) {
		if uk, _ := e.Meta["use_kind"].(string); uk == "cast" {
			t.Errorf("primitive cast `x as number` must emit no cast EdgeTypedAs; got %s", e.To)
		}
	}
}

func TestTSCast_SatisfiesType(t *testing.T) {
	src := `function f(x: unknown) {
	const c = x satisfies AppConfig;
	return c;
}
`
	_, edges := runTSExtract(t, "src/cast.ts", src)
	if !typedAsEdgeTo(edges, "AppConfig", "cast") {
		t.Errorf("expected EdgeTypedAs use_kind=cast → unresolved::AppConfig from satisfies; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
}

func TestTSCast_AngleAssertionPlainTSOnly(t *testing.T) {
	// `<Foo>x` is a type assertion in plain .ts.
	tsSrc := `function f(x: unknown) {
	const el = <ExcalidrawElement>x;
	return el;
}
`
	_, edges := runTSExtract(t, "src/assert.ts", tsSrc)
	if !typedAsEdgeTo(edges, "ExcalidrawElement", "cast") {
		t.Errorf("expected EdgeTypedAs use_kind=cast → unresolved::ExcalidrawElement from <T>x assertion in .ts; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}

	// In .tsx the same text parses as JSX, never a type_assertion, so
	// no cast edge to ExcalidrawElement is emitted from it.
	tsxSrc := `function f(x: unknown) {
	return <Foo>hello</Foo>;
}
`
	_, tsxEdges := runTSExtract(t, "src/assert.tsx", tsxSrc)
	for _, e := range edgesByKind(tsxEdges, graph.EdgeTypedAs) {
		if uk, _ := e.Meta["use_kind"].(string); uk == "cast" && e.To == "unresolved::Foo" {
			t.Errorf("`<Foo>...` in .tsx must not emit a cast EdgeTypedAs (it's JSX); got %s", e.To)
		}
	}
}

func TestTSTypeAliasBody_Union(t *testing.T) {
	src := `type Bar = Foo | Baz;
`
	_, edges := runTSExtract(t, "src/alias.ts", src)
	if !typedAsEdgeTo(edges, "Foo", "type_annotation") {
		t.Errorf("expected EdgeTypedAs → unresolved::Foo from alias body; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
	if !typedAsEdgeTo(edges, "Baz", "type_annotation") {
		t.Errorf("expected EdgeTypedAs → unresolved::Baz from alias body; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
	// The alias never references its own name.
	if typedAsEdgeTo(edges, "Bar", "") {
		t.Errorf("alias body must not self-reference Bar")
	}
	// The edge originates from the alias node, not the file node.
	hasFromAlias := false
	for _, e := range edgesByKind(edges, graph.EdgeTypedAs) {
		if e.To == "unresolved::Foo" && e.From == "src/alias.ts::Bar" {
			hasFromAlias = true
		}
	}
	if !hasFromAlias {
		t.Errorf("alias-body EdgeTypedAs must originate from src/alias.ts::Bar")
	}
}

func TestTSTypeAliasBody_Generic(t *testing.T) {
	src := `type Qux = NonDeleted<ExcalidrawElement>;
`
	_, edges := runTSExtract(t, "src/alias.ts", src)
	if !typedAsEdgeTo(edges, "ExcalidrawElement", "type_annotation") {
		t.Errorf("expected EdgeTypedAs → unresolved::ExcalidrawElement from generic alias body; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
}

func TestTSTypeAliasBody_MapContainerDropped(t *testing.T) {
	src := `type ElementMap = Map<string, ExcalidrawElement>;
`
	_, edges := runTSExtract(t, "src/alias.ts", src)
	if !typedAsEdgeTo(edges, "ExcalidrawElement", "type_annotation") {
		t.Errorf("expected EdgeTypedAs → unresolved::ExcalidrawElement from Map alias body; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
	// Map is a builtin container generic — dropped.
	if typedAsEdgeTo(edges, "Map", "type_annotation") {
		t.Errorf("Map (builtin container) must be dropped from alias-body type refs")
	}
}

func TestTSTypeAliasBody_Intersection(t *testing.T) {
	src := `export type X = A & B;
`
	_, edges := runTSExtract(t, "src/alias.ts", src)
	if !typedAsEdgeTo(edges, "A", "type_annotation") || !typedAsEdgeTo(edges, "B", "type_annotation") {
		t.Errorf("expected EdgeTypedAs → A and B from intersection alias body; got %v",
			edgeTargets(edgesByKind(edges, graph.EdgeTypedAs)))
	}
}

func TestTSTypeRefs_Decompose(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"ExcalidrawElement", []string{"ExcalidrawElement"}},
		{"ExcalidrawElement[]", []string{"ExcalidrawElement"}},
		{"NonDeleted<ExcalidrawElement>", []string{"NonDeleted", "ExcalidrawElement"}},
		{"Map<string, ExcalidrawElement>", []string{"ExcalidrawElement"}},
		{"Foo | Baz", []string{"Foo", "Baz"}},
		{"A & B", []string{"A", "B"}},
		{"Promise<User[]>", []string{"User"}},
		{"number", nil},
		{"string | null", nil},
		{"Record<string, Foo | Bar>", []string{"Foo", "Bar"}},
	}
	for _, c := range cases {
		got := tsTypeRefs(c.in)
		if !sliceEq(got, c.want) {
			t.Errorf("tsTypeRefs(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
