package languages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// vbFind returns the first node with the given name, or nil.
func vbFind(nodes []*graph.Node, name string) *graph.Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func vbHasEdge(edges []*graph.Edge, kind graph.EdgeKind, from, to string) bool {
	for _, e := range edges {
		if e.Kind == kind && (from == "" || e.From == from) && e.To == to {
			return true
		}
	}
	return false
}

func TestVBNetExtractor_Basics(t *testing.T) {
	src := []byte(`Imports System.Data
Imports Alias = System.Text

Namespace Acme.Demo

    Public Class OrderService
        Inherits ServiceBase
        Implements IOrderService, IDisposable

        Public Property OrderCount As Integer

        Public Sub New()
            _count = 0
        End Sub

        Public Function Total(ByVal id As Integer) As Decimal
            Dim repo As New OrderRepository
            Return repo.Sum(id)
        End Function

        Private Sub Log(ByVal msg As String)
            Trace.WriteLine(msg)
        End Sub
    End Class

End Namespace
`)
	e := NewVBNetExtractor()
	require.Equal(t, "vbnet", e.Language())
	require.Equal(t, []string{".vb"}, e.Extensions())

	res, err := e.Extract("Order.vb", src)
	require.NoError(t, err)

	// Namespace is a package node.
	ns := vbFind(res.Nodes, "Acme.Demo")
	require.NotNil(t, ns, "namespace node")
	assert.Equal(t, graph.KindPackage, ns.Kind)

	// Class carries flavor, visibility and enclosing namespace.
	cls := vbFind(res.Nodes, "OrderService")
	require.NotNil(t, cls, "class node")
	assert.Equal(t, graph.KindType, cls.Kind)
	assert.Equal(t, "class", cls.Meta["type_flavor"])
	assert.Equal(t, VisibilityPublic, cls.Meta["visibility"])
	assert.Equal(t, "Acme.Demo", cls.Meta["scope_ns"])
	// End Class is line 24 of the fixture; the range must be real, not a point.
	assert.Greater(t, cls.EndLine, cls.StartLine)
	assert.Equal(t, 24, cls.EndLine, "class extent ends at End Class")

	// Members are methods owned by the class, not free functions.
	total := vbFind(res.Nodes, "Total")
	require.NotNil(t, total, "Total method")
	assert.Equal(t, graph.KindMethod, total.Kind)
	assert.Equal(t, "OrderService", total.Meta["receiver"])
	assert.Greater(t, total.EndLine, total.StartLine)
	assert.Equal(t, "Order.vb::OrderService.Total", total.ID)

	logM := vbFind(res.Nodes, "Log")
	require.NotNil(t, logM, "Log method")
	assert.Equal(t, VisibilityPrivate, logM.Meta["visibility"])

	// Constructor is keyed on <init> rather than "New".
	ctor := vbFind(res.Nodes, "OrderService.<init>")
	require.NotNil(t, ctor, "constructor node")
	assert.Equal(t, graph.KindMethod, ctor.Kind)
	assert.Equal(t, "Order.vb::OrderService.<init>", ctor.ID)

	// Property becomes a field member.
	prop := vbFind(res.Nodes, "OrderCount")
	require.NotNil(t, prop, "property node")
	assert.Equal(t, graph.KindField, prop.Kind)

	// Structural edges.
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeMemberOf, total.ID, cls.ID),
		"Total MEMBER_OF OrderService")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeDefines, "Order.vb", cls.ID),
		"file DEFINES OrderService")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeImports, "Order.vb", "unresolved::import::System.Data"),
		"Imports System.Data")
	// An aliased import records the target namespace, not the alias.
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeImports, "Order.vb", "unresolved::import::System.Text"),
		"aliased Imports resolves to the namespace")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeReferences, cls.ID, "unresolved::ServiceBase"),
		"Inherits ServiceBase")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeImplements, cls.ID, "unresolved::IOrderService"),
		"Implements IOrderService")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeImplements, cls.ID, "unresolved::IDisposable"),
		"second interface on the same Implements clause")

	// Qualified call attributed to its enclosing member.
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeCalls, total.ID, "unresolved::Sum"),
		"repo.Sum() attributed to Total")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeCalls, logM.ID, "unresolved::WriteLine"),
		"Trace.WriteLine() attributed to Log")
}

// VB is case-insensitive: keywords and terminators must match in any casing.
func TestVBNetExtractor_CaseInsensitive(t *testing.T) {
	src := []byte(`MODULE Helpers
    PUBLIC FUNCTION Widen(ByVal s As String) As String
        Return s
    END FUNCTION
END MODULE
`)
	res, err := NewVBNetExtractor().Extract("h.vb", src)
	require.NoError(t, err)

	mod := vbFind(res.Nodes, "Helpers")
	require.NotNil(t, mod)
	assert.Equal(t, graph.KindModule, mod.Kind)

	fn := vbFind(res.Nodes, "Widen")
	require.NotNil(t, fn)
	assert.Equal(t, graph.KindMethod, fn.Kind)
	// END FUNCTION in caps must still close the block.
	assert.Equal(t, 4, fn.EndLine)
}

// Overloads and same-named members of different types must not collapse onto
// one node — the failure mode a flat filePath::name ID scheme produces.
func TestVBNetExtractor_OverloadsStayDistinct(t *testing.T) {
	src := []byte(`Public Class A
    Public Sub Run()
    End Sub
    Public Sub Run(ByVal n As Integer)
    End Sub
End Class

Public Class B
    Public Sub Run()
    End Sub
End Class
`)
	res, err := NewVBNetExtractor().Extract("d.vb", src)
	require.NoError(t, err)

	var runs int
	ids := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Name == "Run" {
			runs++
			ids[n.ID] = true
		}
	}
	assert.Equal(t, 3, runs, "two A.Run overloads plus B.Run")
	assert.Len(t, ids, 3, "every Run has a distinct ID")

	var aReceiver, bReceiver int
	for _, n := range res.Nodes {
		if n.Name != "Run" {
			continue
		}
		switch n.Meta["receiver"] {
		case "A":
			aReceiver++
		case "B":
			bReceiver++
		}
	}
	assert.Equal(t, 2, aReceiver)
	assert.Equal(t, 1, bReceiver)
}

// A Sub at file scope is a free function; the same Sub inside a type is a
// method. VB's script-style files rely on the former.
func TestVBNetExtractor_FileScopeIsFreeFunction(t *testing.T) {
	src := []byte(`Public Sub Main()
    Console.WriteLine("hi")
End Sub
`)
	res, err := NewVBNetExtractor().Extract("m.vb", src)
	require.NoError(t, err)

	main := vbFind(res.Nodes, "Main")
	require.NotNil(t, main)
	assert.Equal(t, graph.KindFunction, main.Kind)
	assert.Nil(t, main.Meta["receiver"])
	assert.Equal(t, "m.vb::Main", main.ID)
}

// Bodyless declarations: interface members, MustOverride members and
// auto-properties have no End keyword, so the extent is the declaration line.
func TestVBNetExtractor_BodylessDeclarations(t *testing.T) {
	src := []byte(`Public Interface IShape
    Function Area() As Double
    Property Name As String
End Interface
`)
	res, err := NewVBNetExtractor().Extract("i.vb", src)
	require.NoError(t, err)

	iface := vbFind(res.Nodes, "IShape")
	require.NotNil(t, iface)
	assert.Equal(t, graph.KindInterface, iface.Kind)

	area := vbFind(res.Nodes, "Area")
	require.NotNil(t, area)
	assert.Equal(t, graph.KindMethod, area.Kind)
	// No `End Function`: start and end collapse to the declaration line.
	assert.Equal(t, area.StartLine, area.EndLine)
}

// Delegate is a type; Declare is an external P/Invoke entry point. Neither may
// be emitted as an ordinary Sub/Function despite matching that modifier run.
func TestVBNetExtractor_DelegateAndDeclare(t *testing.T) {
	src := []byte(`Public Delegate Sub Notify(ByVal msg As String)
Public Declare Function GetTickCount Lib "kernel32" () As Long
`)
	res, err := NewVBNetExtractor().Extract("p.vb", src)
	require.NoError(t, err)

	notify := vbFind(res.Nodes, "Notify")
	require.NotNil(t, notify)
	assert.Equal(t, graph.KindType, notify.Kind)
	assert.Equal(t, "delegate", notify.Meta["type_flavor"])

	tick := vbFind(res.Nodes, "GetTickCount")
	require.NotNil(t, tick)
	assert.Equal(t, graph.KindFunction, tick.Kind)
	assert.Equal(t, true, tick.Meta["external"])

	// Exactly one node per declaration — the Declare must not also surface via
	// the Function pattern.
	var count int
	for _, n := range res.Nodes {
		if n.Name == "GetTickCount" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

// A declaration-free file — the shape of classic-ASP-style VB script — yields
// the file node alone and must not panic.
func TestVBNetExtractor_NoDeclarations(t *testing.T) {
	src := []byte(`Dim x As Integer = 1
Response.Write("hello")
If x > 0 Then
    x = x + 1
End If
`)
	res, err := NewVBNetExtractor().Extract("s.vb", src)
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	// With no enclosing callable there is nothing to attribute a call to.
	for _, e := range res.Edges {
		assert.NotEqual(t, graph.EdgeCalls, e.Kind)
	}
}

func TestVBNetExtractor_EmptyInput(t *testing.T) {
	res, err := NewVBNetExtractor().Extract("e.vb", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

// vbSnapshot renders an extraction into a stable, comparable form: every node
// and edge in emission order with the fields a consumer keys on. Used both to
// assert LF/CRLF equivalence and to pin determinism.
func vbSnapshot(res *parser.ExtractionResult) string {
	var b strings.Builder
	for _, n := range res.Nodes {
		fmt.Fprintf(&b, "N\t%s\t%s\t%s\t%d\t%d\t%v\t%v\t%v\t%v\n",
			n.ID, n.Kind, n.Name, n.StartLine, n.EndLine,
			n.Meta["receiver"], n.Meta["visibility"], n.Meta["scope_ns"],
			n.Meta["methods"])
	}
	for _, e := range res.Edges {
		fmt.Fprintf(&b, "E\t%s\t%s\t%s\t%d\n", e.Kind, e.From, e.To, e.Line)
	}
	return b.String()
}

// The corpus this extractor was built for is Windows-origin: real .vb files are
// CRLF. Every other fixture in this file is LF, so without this test the CRLF
// path is entirely unexercised. helpers_indent.trimmed strips \r for block-end
// detection; the declaration patterns capture \w+ so no name absorbs one. The
// contract is therefore the strong one: CRLF input must produce a byte-identical
// extraction to the same source with LF endings, including line numbers.
func TestVBNetExtractor_CRLFEquivalence(t *testing.T) {
	lf := `Imports System.Data

Namespace Acme.Demo

    Public Class OrderService
        Inherits ServiceBase
        Implements IOrderService, IDisposable

        Public Property OrderCount As Integer

        Public Sub New()
            _count = 0
        End Sub

        Public Function Total(ByVal id As Integer) As Decimal
            Dim repo As New OrderRepository
            Return repo.Sum(id)
        End Function
    End Class

End Namespace
`
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	require.NotEqual(t, lf, crlf, "fixture must actually differ in line endings")
	require.Contains(t, crlf, "\r\n")

	e := NewVBNetExtractor()
	resLF, err := e.Extract("Order.vb", []byte(lf))
	require.NoError(t, err)
	resCRLF, err := e.Extract("Order.vb", []byte(crlf))
	require.NoError(t, err)

	assert.Equal(t, vbSnapshot(resLF), vbSnapshot(resCRLF),
		"CRLF input must extract identically to LF input")

	// Spot-check the line numbers directly rather than trusting only the
	// snapshot equality, so a regression that shifts BOTH consistently is
	// still caught.
	cls := vbFind(resCRLF.Nodes, "OrderService")
	require.NotNil(t, cls)
	assert.Equal(t, 5, cls.StartLine)
	assert.Equal(t, 19, cls.EndLine, "End Class line under CRLF")
	total := vbFind(resCRLF.Nodes, "Total")
	require.NotNil(t, total)
	assert.Equal(t, 15, total.StartLine)
	assert.Equal(t, 18, total.EndLine, "End Function line under CRLF")
}

// Mixed and lone-CR endings must not shift line numbering either. A lone \r is
// NOT a line separator for this extractor (lines are split on \n), which is the
// correct reading for .NET tooling; the assertion documents that.
func TestVBNetExtractor_MixedLineEndings(t *testing.T) {
	src := []byte("Public Class A\r\n    Public Sub One()\r\n    End Sub\n    Public Sub Two()\n    End Sub\r\nEnd Class\r\n")
	res, err := NewVBNetExtractor().Extract("mix.vb", src)
	require.NoError(t, err)

	one := vbFind(res.Nodes, "One")
	require.NotNil(t, one)
	assert.Equal(t, 2, one.StartLine)
	assert.Equal(t, 3, one.EndLine)

	two := vbFind(res.Nodes, "Two")
	require.NotNil(t, two)
	assert.Equal(t, 4, two.StartLine)
	assert.Equal(t, 5, two.EndLine)

	cls := vbFind(res.Nodes, "A")
	require.NotNil(t, cls)
	assert.Equal(t, 6, cls.EndLine)
}

// Encoding normalisation is the indexer's job, not the extractor's: the
// transform pipeline runs bomStripTransform on EVERY file before any extractor
// sees it (internal/indexer/transform.go), and no extractor in this package
// strips a BOM itself. This test pins both halves of that contract so the
// coupling is explicit rather than assumed:
//
//   - After the pipeline's strip, a BOM'd file extracts identically to a clean
//     one. This is the production path, and it is what must never regress.
//   - Handed raw BOM'd bytes directly, a declaration sitting on line 1 does not
//     match, because `^[ \t]*` cannot skip the three BOM bytes. Declarations on
//     any later line are unaffected.
//
// Real .vb files in the corpus this was built for open with `Imports` or a
// comment, and sibling .aspx files are the ones carrying BOMs, so the raw-bytes
// case is a direct-API-caller concern rather than a production one. It is
// deliberately not fixed inside this extractor: doing so here alone would put
// encoding normalisation in one language instead of the shared pipeline that
// already owns it for all of them.
func TestVBNetExtractor_LeadingBOM(t *testing.T) {
	const utf8BOM = "\xEF\xBB\xBF"
	// Line 1 is a comment, which is the common shape for a real .vb file and
	// means nothing extractable sits on the line the BOM occupies.
	body := `' Copyright header
Imports System

Public Class Widget
    Public Sub Go()
        Trace.WriteLine("x")
    End Sub
End Class
`
	e := NewVBNetExtractor()

	clean, err := e.Extract("w.vb", []byte(body))
	require.NoError(t, err)
	withBOM, err := e.Extract("w.vb", []byte(utf8BOM+body))
	require.NoError(t, err)

	// Everything below line 1 is untouched by the BOM: same nodes, same edges,
	// same line numbers. This is the assertion that could genuinely fail --
	// the BOM shifts every byte offset in the file by three.
	assert.NotNil(t, vbFind(withBOM.Nodes, "Widget"), "class survives a BOM")
	assert.NotNil(t, vbFind(withBOM.Nodes, "Go"), "member survives a BOM")
	assert.Equal(t, vbSnapshot(clean), vbSnapshot(withBOM),
		"a BOM must not perturb declarations below line 1")

	// Raw bytes with something extractable ON line 1: the BOM masks it, because
	// `^[ \t]*` cannot skip the three BOM bytes. Documents the extractor's
	// dependence on the indexer's bomStripTransform.
	masked, err := e.Extract("f.vb", []byte(utf8BOM+"Imports System\nPublic Class First\nEnd Class\n"))
	require.NoError(t, err)
	assert.False(t, vbHasEdge(masked.Edges, graph.EdgeImports, "f.vb", "unresolved::import::System"),
		"line-1 Imports is masked by a raw BOM; the indexer strips it first")
	// Degradation stays local to line 1 -- line 2 onward still extracts.
	assert.NotNil(t, vbFind(masked.Nodes, "First"),
		"declaration on line 2 is unaffected")
}

// Same input twice must yield identical IDs, kinds, ordering and line numbers.
// Worth pinning specifically because disambiguateID's _L<line> suffix depends on
// the order collisions are encountered, and because Go map iteration order is
// randomised -- any future change that derives emission order from a map would
// break here rather than silently producing an unstable graph.
func TestVBNetExtractor_Deterministic(t *testing.T) {
	src := []byte(`Public Class A
    Public Sub Run()
    End Sub
    Public Sub Run(ByVal n As Integer)
    End Sub
    Public Sub New()
    End Sub
    Public Property Name As String
End Class

Public Class B
    Public Sub Run()
    End Sub
    Public Sub New()
    End Sub
End Class

Public Module M
    Public Function Helper() As Integer
        Return Util.Compute(1)
    End Function
End Module

Public Interface IWork
    Sub Start()
    Function Stop() As Boolean
End Interface
`)
	e := NewVBNetExtractor()
	first, err := e.Extract("d.vb", src)
	require.NoError(t, err)
	want := vbSnapshot(first)

	// Sanity: the fixture must actually exercise the collision path, otherwise
	// this test would pass trivially.
	require.Contains(t, want, "_L", "fixture must produce at least one _L-disambiguated ID")

	for i := 0; i < 25; i++ {
		res, err := e.Extract("d.vb", src)
		require.NoError(t, err)
		require.Equal(t, want, vbSnapshot(res), "extraction differed on pass %d", i+2)
	}

	// A fresh extractor instance must agree too -- no state may persist on the
	// receiver between files.
	other, err := NewVBNetExtractor().Extract("d.vb", src)
	require.NoError(t, err)
	assert.Equal(t, want, vbSnapshot(other))
}

// Table-driven coverage of the modifier runs. The declaration patterns accept
// the modifier keywords in any order and any combination, so the risk is a
// modifier that silently prevents a match (or is captured as the member name).
func TestVBNetExtractor_ModifierMatrix(t *testing.T) {
	cases := []struct {
		name       string
		decl       string
		wantName   string
		wantKind   graph.NodeKind
		wantVis    string
		wantInType bool
	}{
		{"public sub", "Public Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPublic, true},
		{"private sub", "Private Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPrivate, true},
		{"protected sub", "Protected Sub Alpha()", "Alpha", graph.KindMethod, VisibilityProtected, true},
		{"friend maps to internal", "Friend Sub Alpha()", "Alpha", graph.KindMethod, VisibilityInternal, true},
		{"protected friend", "Protected Friend Sub Alpha()", "Alpha", graph.KindMethod, VisibilityProtected, true},
		{"shared", "Public Shared Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPublic, true},
		{"overrides", "Public Overrides Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPublic, true},
		{"overridable", "Public Overridable Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPublic, true},
		{"notoverridable", "Public NotOverridable Overrides Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPublic, true},
		{"shadows", "Public Shadows Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPublic, true},
		{"partial", "Private Partial Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPrivate, true},
		{"async", "Public Async Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPublic, true},
		{"shared before visibility", "Shared Public Sub Alpha()", "Alpha", graph.KindMethod, "", true},
		{"no modifier at all", "Sub Alpha()", "Alpha", graph.KindMethod, "", true},
		{"long run", "Public Shared Shadows Overridable Sub Alpha()", "Alpha", graph.KindMethod, VisibilityPublic, true},

		{"function", "Public Function Beta() As Integer", "Beta", graph.KindMethod, VisibilityPublic, true},
		{"async function", "Public Async Function Beta() As Task", "Beta", graph.KindMethod, VisibilityPublic, true},
		{"iterator function", "Public Iterator Function Beta() As IEnumerable", "Beta", graph.KindMethod, VisibilityPublic, true},
		{"mustoverride function", "Protected MustOverride Function Beta() As Integer", "Beta", graph.KindMethod, VisibilityProtected, true},
		{"shared function", "Friend Shared Function Beta() As Integer", "Beta", graph.KindMethod, VisibilityInternal, true},

		{"property", "Public Property Gamma As String", "Gamma", graph.KindField, VisibilityPublic, true},
		{"readonly property", "Public ReadOnly Property Gamma As String", "Gamma", graph.KindField, VisibilityPublic, true},
		{"writeonly property", "Public WriteOnly Property Gamma As String", "Gamma", graph.KindField, VisibilityPublic, true},
		{"default property", "Public Default Property Gamma As String", "Gamma", graph.KindField, VisibilityPublic, true},
		{"shared readonly property", "Private Shared ReadOnly Property Gamma As String", "Gamma", graph.KindField, VisibilityPrivate, true},
		{"overrides property", "Public Overrides ReadOnly Property Gamma As String", "Gamma", graph.KindField, VisibilityPublic, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte("Public Class Host\n    " + tc.decl + "\nEnd Class\n")
			res, err := NewVBNetExtractor().Extract("mods.vb", src)
			require.NoError(t, err)

			n := vbFind(res.Nodes, tc.wantName)
			require.NotNil(t, n, "declaration %q produced no node", tc.decl)
			assert.Equal(t, tc.wantKind, n.Kind)
			if tc.wantVis == "" {
				assert.Nil(t, n.Meta["visibility"],
					"no leading access modifier must not be guessed")
			} else {
				assert.Equal(t, tc.wantVis, n.Meta["visibility"])
			}
			if tc.wantInType {
				assert.Equal(t, "Host", n.Meta["receiver"])
				assert.Equal(t, "mods.vb::Host."+tc.wantName, n.ID)
			}
		})
	}
}

// Case-insensitive modifier runs: VB source in the wild mixes casing freely.
func TestVBNetExtractor_ModifierCasing(t *testing.T) {
	for _, decl := range []string{
		"PUBLIC SHARED SUB Alpha()",
		"public shared sub Alpha()",
		"Public SHARED Sub Alpha()",
		"pUbLiC sHaReD sUb Alpha()",
	} {
		src := []byte("Public Class Host\n    " + decl + "\nEnd Class\n")
		res, err := NewVBNetExtractor().Extract("c.vb", src)
		require.NoError(t, err)
		n := vbFind(res.Nodes, "Alpha")
		require.NotNil(t, n, "declaration %q produced no node", decl)
		assert.Equal(t, graph.KindMethod, n.Kind)
		assert.Equal(t, VisibilityPublic, n.Meta["visibility"])
	}
}

// `New X.Y(...)` is a type instantiation. Emitting it as a call to a member
// named Y is the single largest source of false CALLS edges in designer-heavy
// VB (measured: thousands on a real estate), so the filter is load-bearing and
// gets its own guard.
func TestVBNetExtractor_NewIsNotACall(t *testing.T) {
	src := []byte(`Public Class Form1
    Public Sub Build()
        Me.Size = New System.Drawing.Size(120, 40)
        Dim list As New Collections.Generic.List(Of String)()
        Me.Panel.Controls.Add(list)
    End Sub
End Class
`)
	res, err := NewVBNetExtractor().Extract("f.vb", src)
	require.NoError(t, err)

	build := vbFind(res.Nodes, "Build")
	require.NotNil(t, build)

	// The instantiated type is recorded as a reference...
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeReferences, "f.vb", "unresolved::Size"),
		"New System.Drawing.Size(...) recorded as a type reference")
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeReferences, "f.vb", "unresolved::List"),
		"New ...List(Of String) recorded as a type reference")

	// ...and must NOT appear as a call to a member named Size.
	assert.False(t, vbHasEdge(res.Edges, graph.EdgeCalls, build.ID, "unresolved::Size"),
		"New ...Size(...) must not emit a CALLS edge")

	// A genuine qualified invocation on the same body still lands.
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeCalls, build.ID, "unresolved::Add"),
		"Controls.Add(list) is a real call")
}

// KNOWN LIMITATION, pinned deliberately rather than left implicit.
//
// VB uses `(` for indexed/default property access as well as invocation, so
// `dt.Rows(0)` is syntactically indistinguishable from a method call without
// type information this extractor does not have. Such accesses are therefore
// still emitted as CALLS edges. A hardcoded accessor blocklist (Rows/Cells/
// Fields/...) was rejected as too framework-specific to be correct upstream.
//
// This test asserts the CURRENT behaviour so the limitation is visible in the
// suite and any future fix shows up as an intentional change here.
func TestVBNetExtractor_IndexedPropertyAccessIsEmittedAsCall(t *testing.T) {
	src := []byte(`Public Class Repo
    Public Sub Scan(ByVal dt As DataTable)
        Dim v As Object = dt.Rows(0)
    End Sub
End Class
`)
	res, err := NewVBNetExtractor().Extract("r.vb", src)
	require.NoError(t, err)

	scan := vbFind(res.Nodes, "Scan")
	require.NotNil(t, scan)
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeCalls, scan.ID, "unresolved::Rows"),
		"indexed property access is currently emitted as a call (documented limitation)")
}

// Malformed, truncated and non-UTF8 input must not panic, and degradation must
// stay local: the extractor should still return the file node and whatever
// declarations it could recognise.
func TestVBNetExtractor_Robustness(t *testing.T) {
	cases := []struct {
		name string
		src  []byte
	}{
		{"class with no End Class at EOF", []byte("Public Class Orphan\n    Public Sub M()\n    End Sub\n")},
		{"sub with no End Sub at EOF", []byte("Public Class A\n    Public Sub M()\n        x = 1\n")},
		{"truncated mid-declaration", []byte("Public Class A\n    Public Sub Half(")},
		{"truncated mid-keyword", []byte("Public Cla")},
		{"only comments", []byte("' first\n' second\nREM third\n")},
		{"only whitespace", []byte("   \n\t\n \n")},
		{"no trailing newline", []byte("Public Class A\nEnd Class")},
		{"lone CR only", []byte("Public Class A\rEnd Class\r")},
		{"nul bytes", []byte("Public Class A\x00\n    Public Sub M()\x00\n    End Sub\nEnd Class\n")},
		{"invalid utf8 latin1", []byte("Public Class Caf\xE9\n    Public Sub M()\n    End Sub\nEnd Class\n")},
		{"invalid utf8 mid-body", append([]byte("Public Class A\n    Public Sub M()\n        s = \""), append([]byte{0xFF, 0xFE, 0xFD}, []byte("\"\n    End Sub\nEnd Class\n")...)...)},
		{"utf16 bom then garbage", append([]byte{0xFF, 0xFE}, []byte("P\x00u\x00b\x00l\x00i\x00c\x00")...)},
		{"unterminated namespace", []byte("Namespace A.B\n    Public Class C\n")},
		{"End without opener", []byte("End Class\nEnd Sub\nEnd Namespace\n")},
		{"deeply nested unterminated", []byte(strings.Repeat("Namespace N\n", 200))},
		{"very long single line", []byte("Public Class A\n    Public Sub M()\n        x = " + strings.Repeat("obj.Call(1) + ", 2000) + "0\n    End Sub\nEnd Class\n")},
		{"many overloads colliding", []byte("Public Class A\n" + strings.Repeat("    Public Sub Dup()\n    End Sub\n", 500) + "End Class\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var res *parser.ExtractionResult
			var err error
			require.NotPanics(t, func() {
				res, err = NewVBNetExtractor().Extract("x.vb", tc.src)
			}, "extractor panicked")
			require.NoError(t, err)
			require.NotNil(t, res)

			// The file node is always present and is always first.
			require.GreaterOrEqual(t, len(res.Nodes), 1)
			assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)

			// Structural invariants that must hold for ANY input.
			ids := map[string]bool{}
			for _, n := range res.Nodes {
				assert.NotEmpty(t, n.ID, "node with empty ID")
				assert.False(t, ids[n.ID], "duplicate node ID %q", n.ID)
				ids[n.ID] = true
				assert.GreaterOrEqual(t, n.StartLine, 1, "node %q StartLine", n.ID)
				assert.GreaterOrEqual(t, n.EndLine, n.StartLine,
					"node %q EndLine before StartLine", n.ID)
			}
			for _, e := range res.Edges {
				assert.NotEmpty(t, e.From, "edge with empty From")
				assert.NotEmpty(t, e.To, "edge with empty To")
				// Every edge originating inside this file must name a node the
				// extraction actually emitted -- no dangling internal source.
				if strings.HasPrefix(e.From, "x.vb") {
					assert.True(t, ids[e.From], "edge From %q names no emitted node", e.From)
				}
			}
		})
	}
}

// An unterminated block must degrade to a point extent rather than swallowing
// the rest of the file or producing an inverted range.
func TestVBNetExtractor_UnterminatedBlockExtent(t *testing.T) {
	src := []byte("Public Class Orphan\n    Public Sub M()\n        x = 1\n")
	res, err := NewVBNetExtractor().Extract("o.vb", src)
	require.NoError(t, err)

	cls := vbFind(res.Nodes, "Orphan")
	require.NotNil(t, cls)
	assert.Equal(t, cls.StartLine, cls.EndLine,
		"no End Class: extent collapses to the declaration line")

	m := vbFind(res.Nodes, "M")
	require.NotNil(t, m)
	assert.Equal(t, m.StartLine, m.EndLine,
		"no End Sub: extent collapses to the declaration line")
	// With the class extent collapsed, M no longer sits inside it, so it is a
	// free function. Documents the degradation shape.
	assert.Equal(t, graph.KindFunction, m.Kind)
}

// Interface method sets feed the resolver's IMPLEMENTS inference, which reads
// Meta["methods"] rather than member edges. csharp.go stamps this and VB
// mirrors it, so a VB interface supports the same inference as a C# one.
//
// Only Sub and Function belong to that set: a Property is emitted as a field
// and an Event as an event, and neither is part of an interface's callable
// contract for this purpose.
func TestVBNetExtractor_InterfaceMethodSet(t *testing.T) {
	src := []byte(`Public Interface IShape
    Function Area() As Double
    Sub Draw()
    Property Name As String
    Event Changed()
End Interface

Public Interface IStore
    Sub Save()
End Interface

Public Class Widget
    Public Sub Helper()
    End Sub
End Class
`)
	res, err := NewVBNetExtractor().Extract("i.vb", src)
	require.NoError(t, err)

	shape := vbFind(res.Nodes, "IShape")
	require.NotNil(t, shape)
	require.Equal(t, graph.KindInterface, shape.Kind)

	methods, ok := shape.Meta["methods"].([]string)
	require.True(t, ok, `IShape must carry a []string Meta["methods"]`)
	assert.ElementsMatch(t, []string{"Area", "Draw"}, methods)
	assert.NotContains(t, methods, "Name", "a Property is not part of the method set")
	assert.NotContains(t, methods, "Changed", "an Event is not part of the method set")

	// A second interface in the same file keeps its own set -- the map is
	// keyed on node ID, so same-named members across interfaces cannot merge.
	store := vbFind(res.Nodes, "IStore")
	require.NotNil(t, store)
	assert.Equal(t, []string{"Save"}, store.Meta["methods"])

	// A class is never stamped, even though its members are methods.
	widget := vbFind(res.Nodes, "Widget")
	require.NotNil(t, widget)
	assert.Equal(t, graph.KindType, widget.Kind)
	assert.Nil(t, widget.Meta["methods"], "only interfaces carry a method set")
}

// A marker interface has no methods; the key must be absent rather than an
// empty slice, so a consumer can tell "no methods" from "not extracted".
func TestVBNetExtractor_EmptyInterfaceHasNoMethodSet(t *testing.T) {
	res, err := NewVBNetExtractor().Extract("m.vb", []byte(`Public Interface IMarker
End Interface
`))
	require.NoError(t, err)

	iface := vbFind(res.Nodes, "IMarker")
	require.NotNil(t, iface)
	assert.Equal(t, graph.KindInterface, iface.Kind)
	assert.Nil(t, iface.Meta["methods"])
}

// Nested containers. findKeywordBlockEnd stops at the FIRST terminator, which
// for a nested class is the inner one -- truncating the outer extent so every
// member declared after the nested block loses its owner and its MEMBER_OF
// edge. vbContainerEnd counts depth instead.
// Namespaces nest too, and the outer one truncating at the inner terminator
// costs every type declared after that inner block its scope_ns. Milder than
// the class case -- no edge is lost, since types are not members of a
// namespace -- but the same defect.
func TestVBNetExtractor_NestedNamespaceExtent(t *testing.T) {
	src := []byte(`Namespace Outer

    Namespace Inner
        Public Class InnerType
        End Class
    End Namespace

    Public Class OuterType
    End Class

End Namespace
`)
	res, err := NewVBNetExtractor().Extract("ns.vb", src)
	require.NoError(t, err)

	outer := vbFind(res.Nodes, "Outer")
	require.NotNil(t, outer)
	assert.Equal(t, 1, outer.StartLine)
	assert.Equal(t, 11, outer.EndLine, "outer namespace must span past the nested End Namespace")

	inner := vbFind(res.Nodes, "Inner")
	require.NotNil(t, inner)
	assert.Equal(t, 3, inner.StartLine)
	assert.Equal(t, 6, inner.EndLine)

	// The type after the nested block is the one that regressed.
	ot := vbFind(res.Nodes, "OuterType")
	require.NotNil(t, ot)
	assert.Equal(t, "Outer", ot.Meta["scope_ns"])

	// The inner type still resolves to the innermost namespace, not the outer
	// one, now that both cover it.
	it := vbFind(res.Nodes, "InnerType")
	require.NotNil(t, it)
	assert.Equal(t, "Inner", it.Meta["scope_ns"])
}

func TestVBNetExtractor_NestedContainerExtent(t *testing.T) {
	src := []byte(`Public Class Outer
    Public Class Inner
        Public Sub InnerMethod()
        End Sub
    End Class

    Public Sub OuterMethod()
    End Sub
End Class
`)
	res, err := NewVBNetExtractor().Extract("n.vb", src)
	require.NoError(t, err)

	outer := vbFind(res.Nodes, "Outer")
	require.NotNil(t, outer)
	assert.Equal(t, 1, outer.StartLine)
	assert.Equal(t, 9, outer.EndLine, "outer extent must span past the nested End Class")

	inner := vbFind(res.Nodes, "Inner")
	require.NotNil(t, inner)
	assert.Equal(t, 2, inner.StartLine)
	assert.Equal(t, 5, inner.EndLine)

	// The member after the nested block is the one that regressed.
	om := vbFind(res.Nodes, "OuterMethod")
	require.NotNil(t, om)
	assert.Equal(t, graph.KindMethod, om.Kind)
	assert.Equal(t, "Outer", om.Meta["receiver"])
	assert.Equal(t, "n.vb::Outer.OuterMethod", om.ID)
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeMemberOf, om.ID, outer.ID),
		"OuterMethod MEMBER_OF Outer")

	// The inner member still belongs to the inner class, not the outer one.
	im := vbFind(res.Nodes, "InnerMethod")
	require.NotNil(t, im)
	assert.Equal(t, "Inner", im.Meta["receiver"])
	assert.True(t, vbHasEdge(res.Edges, graph.EdgeMemberOf, im.ID, inner.ID))
}

// A Declare is only valid inside a Class, Structure or Module, so a valid one
// always has an owner. Emitting it at file scope dropped that owner and its
// MEMBER_OF edge on every real declaration.
func TestVBNetExtractor_DeclareInsideContainer(t *testing.T) {
	src := []byte(`Public Module NativeApi
    Public Declare Function GetTickCount Lib "kernel32" () As Long
    Public Declare Sub SleepFor Lib "kernel32" (ByVal ms As Long)
End Module
`)
	res, err := NewVBNetExtractor().Extract("d.vb", src)
	require.NoError(t, err)

	mod := vbFind(res.Nodes, "NativeApi")
	require.NotNil(t, mod)

	for _, name := range []string{"GetTickCount", "SleepFor"} {
		n := vbFind(res.Nodes, name)
		require.NotNil(t, n, name)
		assert.Equal(t, graph.KindMethod, n.Kind, name)
		assert.Equal(t, "NativeApi", n.Meta["receiver"], name)
		assert.Equal(t, true, n.Meta["external"], name+" keeps its external marker")
		assert.Equal(t, "d.vb::NativeApi."+name, n.ID)
		assert.True(t, vbHasEdge(res.Edges, graph.EdgeMemberOf, n.ID, mod.ID), name)
		// Bodyless: the extent is the declaration line itself.
		assert.Equal(t, n.StartLine, n.EndLine, name)
	}
}
