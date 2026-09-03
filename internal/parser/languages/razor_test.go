package languages

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestRazorExtractor(t *testing.T) {
	const razor = `@page "/counter"
@inherits ComponentBase
@inject IWeatherService Weather

<h1>Counter</h1>
<button @onclick="Increment">Click</button>

@code {
    private int count = 0;
    private void Increment()
    {
        count++;
    }
}
`
	res, err := NewRazorExtractor().Extract("Counter.razor", []byte(razor))
	if err != nil {
		t.Fatal(err)
	}

	var incr *graph.Node
	refs := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Name == "Increment" {
			incr = n
		}
		if n.Name == "__RazorCode" {
			t.Errorf("synthetic wrapper class leaked into the graph: %+v", n)
		}
	}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeReferences && strings.HasPrefix(e.To, "unresolved::") {
			refs[strings.TrimPrefix(e.To, "unresolved::")] = true
		}
	}

	// The @code block's C# member is extracted, rebased into host coordinates.
	if incr == nil {
		t.Fatalf("@code method 'Increment' was not extracted from the Razor file")
	}
	if incr.Language != "razor" || incr.Meta["inline_script"] != true {
		t.Errorf("delegated symbol lang=%q meta=%v, want razor + inline_script", incr.Language, incr.Meta)
	}
	if incr.StartLine != 10 {
		t.Errorf("Increment StartLine = %d, want 10 (host-file coordinates)", incr.StartLine)
	}

	// @inherits and @inject directives become type references.
	for _, want := range []string{"ComponentBase", "IWeatherService"} {
		if !refs[want] {
			t.Errorf("missing directive type reference %q (refs: %v)", want, refs)
		}
	}
}

// A @code block's members are rebased into host coordinates; the
// ownership span stamped on a declaring-first partial property (issue
// #731) has to move with the node's lines, or it names unrelated host
// lines and the semantic tier admits stubs from the wrong region.
func TestRazorCodeBlockRebasesOwnershipSpan(t *testing.T) {
	const razor = `@page "/p"

@code {
    private Svc _c = new Svc();
    public partial int P { get; set; }
    public partial int P {
        get { return _c.F(); }
    }
}
`
	res, err := NewRazorExtractor().Extract("Page.razor", []byte(razor))
	if err != nil {
		t.Fatal(err)
	}
	var prop *graph.Node
	for _, n := range res.Nodes {
		if n.Name == "P" && n.Kind == graph.KindField {
			prop = n
		}
	}
	if prop == nil {
		t.Fatal("partial property P not extracted from the @code block")
	}
	if prop.StartLine != 5 || prop.EndLine != 5 {
		t.Errorf("P node lines = %d..%d, want 5..5 (host coordinates of the declaring fragment)", prop.StartLine, prop.EndLine)
	}
	if got, want := prop.Meta[graph.MetaOwnershipStartLine], 6; got != want {
		t.Errorf("ownership_start_line = %v, want %d (host coordinates of the implementing fragment)", got, want)
	}
	if got, want := prop.Meta[graph.MetaOwnershipEndLine], 8; got != want {
		t.Errorf("ownership_end_line = %v, want %d", got, want)
	}
	var callLine int
	for _, e := range res.Edges {
		if e.From == prop.ID && e.Kind == graph.EdgeCalls && strings.HasSuffix(e.To, ".F") {
			callLine = e.Line
		}
	}
	if callLine != 7 {
		t.Errorf("P's body call line = %d, want 7 (inside the stamped span, outside the node's)", callLine)
	}
}

// TestRazorUsingExtraction pins `@using` directive extraction (with the
// `@using static` member-import form skipped to its namespace) and the
// path-derived component namespace.
func TestRazorUsingExtraction(t *testing.T) {
	const razor = `@using App.Widgets
@using static System.Math

<Counter />
`
	res, err := NewRazorExtractor().Extract("Components/Page.razor", []byte(razor))
	if err != nil {
		t.Fatal(err)
	}

	usings := map[string]bool{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeImports && strings.HasPrefix(e.To, "unresolved::razor_using::") {
			usings[strings.TrimPrefix(e.To, "unresolved::razor_using::")] = true
		}
	}
	for _, want := range []string{"App.Widgets", "System.Math"} {
		if !usings[want] {
			t.Errorf("missing @using namespace %q (got: %v)", want, usings)
		}
	}

	var comp *graph.Node
	for _, n := range res.Nodes {
		if n.Name == "Page" && n.Meta["component"] == true {
			comp = n
		}
	}
	if comp == nil {
		t.Fatal("Page component node not emitted")
	}
	if comp.Meta["scope_ns"] != "Components" {
		t.Errorf("component scope_ns = %v, want Components", comp.Meta["scope_ns"])
	}
}

// TestRazorBraceMatcherSkipsStringsAndCarvesBareBlock is the B4 named test: a
// `}` inside a C# string or comment in a @code block must not truncate the
// block (which dropped every member after it), and a bare @{ } block is also
// carved and delegated. The per-file Blazor component node is emitted.
func TestRazorBraceMatcherSkipsStringsAndCarvesBareBlock(t *testing.T) {
	src := []byte("<h1>Hi</h1>\n" +
		"@code {\n" +
		"    string Brace() { return \"}\"; }\n" +
		"    void After() { }\n" +
		"    // a comment with a } brace\n" +
		"    void Last() { }\n" +
		"}\n" +
		"@{\n" +
		"    void BareHelper() { }\n" +
		"}\n")
	res, err := NewRazorExtractor().Extract("Counter.razor", src)
	if err != nil {
		t.Fatal(err)
	}

	methods := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindMethod || n.Kind == graph.KindFunction {
			methods[n.Name] = true
		}
	}
	// All three @code members survive — the brace inside the string / comment
	// no longer truncates the block.
	for _, want := range []string{"Brace", "After", "Last"} {
		if !methods[want] {
			t.Fatalf("method %s was dropped by brace truncation; got %v", want, methods)
		}
	}
	// The synthetic wrapper method/class are not leaked as symbols.
	if methods["__Body"] {
		t.Fatalf("synthetic __Body wrapper leaked as a method")
	}

	// The bare @{ } block is carved (two spans total).
	spans := razorCodeSpans(src)
	var bareSeen bool
	for _, s := range spans {
		if s.bare {
			bareSeen = true
		}
	}
	if !bareSeen {
		t.Fatalf("bare @{ } block was not carved; spans=%v", spans)
	}

	// The per-file component node exists and is navigable.
	var component bool
	for _, n := range res.Nodes {
		if n.Kind == graph.KindType && n.ID == "Counter.razor::Counter" {
			if c, _ := n.Meta["component"].(bool); c {
				component = true
			}
		}
	}
	if !component {
		t.Fatalf("expected a per-file component node Counter.razor::Counter")
	}
}

// TestRazorMatchBraceUnit checks the matcher directly on literals and comments.
func TestRazorMatchBraceUnit(t *testing.T) {
	// open at index 0; the } inside the string must be ignored.
	src := []byte(`{ var s = "a}b"; var c = '}'; /* } */ }`)
	end := matchRazorBrace(src, 0)
	if end != len(src)-1 {
		t.Fatalf("matchRazorBrace = %d, want %d (final brace)", end, len(src)-1)
	}
}

func razorTagRef(edges []*graph.Edge, from, name string) *graph.Edge {
	for _, e := range edges {
		if e.Kind == graph.EdgeReferences && e.From == from && e.To == "unresolved::"+name {
			return e
		}
	}
	return nil
}

func razorAnyRefTo(edges []*graph.Edge, name string) bool {
	for _, e := range edges {
		if e.Kind == graph.EdgeReferences && e.To == "unresolved::"+name {
			return true
		}
	}
	return false
}

func TestRazorComponentTagAndGenericArg(t *testing.T) {
	src := []byte(`<h1>Parent</h1>
<Child />
<Grid TItem="CatalogItem" />
@code {
    private List<DecoyType> items = new();
}
`)
	res, err := NewRazorExtractor().Extract("Pages/Parent.razor", src)
	if err != nil {
		t.Fatal(err)
	}
	const comp = "Pages/Parent.razor::Parent"

	if razorTagRef(res.Edges, comp, "Child") == nil {
		t.Errorf("expected a component-tag reference to Child")
	}
	if razorTagRef(res.Edges, comp, "Grid") == nil {
		t.Errorf("expected a component-tag reference to Grid")
	}
	// The TItem generic type-arg references the DTO.
	if !razorAnyRefTo(res.Edges, "CatalogItem") {
		t.Errorf("expected a type reference to the TItem value CatalogItem")
	}
	// The C# generic `List<DecoyType>` inside @code must not be misread as a
	// markup tag — its body is blanked before the tag scan.
	if razorTagRef(res.Edges, comp, "DecoyType") != nil {
		t.Errorf("a generic type-arg inside @code must not become a component-tag reference")
	}
	if razorTagRef(res.Edges, comp, "List") != nil {
		t.Errorf("a generic container inside @code must not become a component-tag reference")
	}
}

func TestRazorBlazorBuiltinsSkipped(t *testing.T) {
	src := []byte(`<EditForm Model="m">
  <InputText @bind-Value="name" />
  <ValidationSummary />
  <Router />
  <MyWidget />
</EditForm>
`)
	res, err := NewRazorExtractor().Extract("Pages/Form.razor", src)
	if err != nil {
		t.Fatal(err)
	}
	const comp = "Pages/Form.razor::Form"
	for _, b := range []string{"EditForm", "InputText", "ValidationSummary", "Router"} {
		if razorTagRef(res.Edges, comp, b) != nil {
			t.Errorf("Blazor framework component %s must not emit a reference", b)
		}
	}
	if razorTagRef(res.Edges, comp, "MyWidget") == nil {
		t.Errorf("user component MyWidget should still be referenced")
	}
}

func TestBlazorBuiltinNotSuppressedInSvelte(t *testing.T) {
	// `EditForm` is a Blazor builtin but a plausible user component elsewhere —
	// the suppression must be scoped to Razor, not leak into other languages.
	res, err := NewSvelteExtractor().Extract("Form.svelte", []byte("<EditForm />\n"))
	if err != nil {
		t.Fatal(err)
	}
	if razorTagRef(res.Edges, "Form.svelte::Form", "EditForm") == nil {
		t.Errorf("a user EditForm component in Svelte must NOT be suppressed (language-scoping)")
	}
}
