package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func phpType(s graph.Store, id, name string, meta map[string]any) {
	s.AddNode(&graph.Node{ID: id, Kind: graph.KindType, Name: name, FilePath: id, Language: "php", Meta: meta})
}
func phpIface(s graph.Store, id, name string, meta map[string]any) {
	s.AddNode(&graph.Node{ID: id, Kind: graph.KindInterface, Name: name, FilePath: id, Language: "php", Meta: meta})
}
func phpMethod(s graph.Store, id, name, receiver string) {
	s.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: name, FilePath: id, Language: "php",
		Meta: map[string]any{"receiver": receiver}})
}

// Interface-typed receiver: an ambiguous `$h->handle()` fans out to the
// interface declaration + every in-repo implementation, related through the
// `implements` clause (scope_interfaces). Edges land at ast_inferred so they
// are visible in default find_usages.
func TestResolvePHPOverrideDispatch_InterfaceFanOut(t *testing.T) {
	var s graph.Store = graph.New()
	iface := "src/HandlerInterface.php"
	th := "src/TestHandler.php"
	sh := "src/StreamHandler.php"
	app := "src/app.php"

	phpIface(s, iface+"::HandlerInterface", "HandlerInterface", nil)
	phpMethod(s, iface+"::HandlerInterface.handle", "handle", "HandlerInterface")
	phpType(s, th+"::TestHandler", "TestHandler", map[string]any{"scope_interfaces": "HandlerInterface"})
	phpMethod(s, th+"::TestHandler.handle", "handle", "TestHandler")
	phpType(s, sh+"::StreamHandler", "StreamHandler", map[string]any{"scope_interfaces": "HandlerInterface"})
	phpMethod(s, sh+"::StreamHandler.handle", "handle", "StreamHandler")

	caller := app + "::run"
	s.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "run", FilePath: app, Language: "php"})
	s.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.handle", Kind: graph.EdgeCalls, FilePath: app, Line: 5})

	r := New(s)
	n := r.resolvePHPOverrideDispatch()
	assert.Positive(t, n, "at least one dispatch edge should be bound")

	for _, target := range []string{
		iface + "::HandlerInterface.handle",
		th + "::TestHandler.handle",
		sh + "::StreamHandler.handle",
	} {
		var found *graph.Edge
		for _, in := range s.GetInEdges(target) {
			if in.From == caller && in.Kind == graph.EdgeCalls {
				found = in
				break
			}
		}
		require.NotNil(t, found, "expected fan-out call edge run→%s", target)
		assert.Equal(t, graph.OriginASTInferred, found.Origin, "dispatch edge is ast_inferred (visible)")
		assert.Equal(t, "override", found.Meta["dispatch"])
		assert.Equal(t, 5, found.Line, "edge anchored at the call site")
	}
	// The ambiguous stub is gone.
	for _, e := range s.GetOutEdges(caller) {
		assert.NotEqual(t, "unresolved::*.handle", e.To)
	}
}

// parent::__construct() must bind through a 3-deep extends chain (Leaf → Mid →
// Base, only Base declares the constructor), a precise single bind.
func TestResolvePHPOverrideDispatch_ParentConstructChain(t *testing.T) {
	var s graph.Store = graph.New()
	base := "src/Base.php"
	mid := "src/Mid.php"
	leaf := "src/Leaf.php"

	phpType(s, base+"::Base", "Base", nil)
	phpMethod(s, base+"::Base.__construct", "__construct", "Base")
	phpType(s, mid+"::Mid", "Mid", map[string]any{"scope_parent": "Base"})
	phpType(s, leaf+"::Leaf", "Leaf", map[string]any{"scope_parent": "Mid"})
	phpMethod(s, leaf+"::Leaf.__construct", "__construct", "Leaf")
	s.AddEdge(&graph.Edge{
		From: mid + "::Mid", To: base + "::Base", Kind: graph.EdgeExtends,
		FilePath: mid, Meta: map[string]any{"target_fqn": "Base"},
	})
	s.AddEdge(&graph.Edge{
		From: leaf + "::Leaf", To: mid + "::Mid", Kind: graph.EdgeExtends,
		FilePath: leaf, Meta: map[string]any{"target_fqn": "Mid"},
	})

	caller := leaf + "::Leaf.__construct"
	s.AddEdge(&graph.Edge{
		From: caller, To: "unresolved::*.__construct", Kind: graph.EdgeCalls,
		FilePath: leaf, Line: 7, Meta: map[string]any{"scope_kind": "parent"},
	})

	r := New(s)
	r.resolvePHPOverrideDispatch()

	var bound *graph.Edge
	for _, e := range s.GetOutEdges(caller) {
		if e.Kind == graph.EdgeCalls && e.To == base+"::Base.__construct" {
			bound = e
			break
		}
	}
	require.NotNil(t, bound, "parent::__construct must bind to Base.__construct up the 3-deep chain")
	assert.Equal(t, graph.OriginASTInferred, bound.Origin)
	assert.Equal(t, "scope", bound.Meta["dispatch"])
	for _, e := range s.GetOutEdges(caller) {
		assert.NotEqual(t, "unresolved::*.__construct", e.To)
	}
}

// A trait-provided method binds via `use`: Button extends Widget, Widget uses
// trait Renderer (an EdgeExtends), so `$x->render()` fans out to Renderer.render
// (the trait) as well as Button.render — related through the trait in the
// ancestor closure.
func TestResolvePHPOverrideDispatch_TraitProvidedMethod(t *testing.T) {
	var s graph.Store = graph.New()
	trait := "src/Renderer.php"
	widget := "src/Widget.php"
	button := "src/Button.php"
	app := "src/paint.php"

	phpType(s, trait+"::Renderer", "Renderer", map[string]any{"type_flavor": "trait", "kind": "trait"})
	phpMethod(s, trait+"::Renderer.render", "render", "Renderer")
	phpType(s, widget+"::Widget", "Widget", nil)
	// Widget uses the Renderer trait — modelled as an EdgeExtends to the stub.
	s.AddEdge(&graph.Edge{From: widget + "::Widget", To: "unresolved::Renderer", Kind: graph.EdgeExtends,
		FilePath: widget, Line: 3, Meta: map[string]any{"via": "trait"}})
	phpType(s, button+"::Button", "Button", map[string]any{"scope_parent": "Widget"})
	phpMethod(s, button+"::Button.render", "render", "Button")

	caller := app + "::paint"
	s.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "paint", FilePath: app, Language: "php"})
	s.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.render", Kind: graph.EdgeCalls, FilePath: app, Line: 4})

	r := New(s)
	r.resolvePHPOverrideDispatch()

	for _, target := range []string{trait + "::Renderer.render", button + "::Button.render"} {
		var found bool
		for _, in := range s.GetInEdges(target) {
			if in.From == caller && in.Kind == graph.EdgeCalls {
				found = true
			}
		}
		assert.True(t, found, "expected fan-out call edge paint→%s (trait-related)", target)
	}
}

// A phpdoc @method virtual node joins the member-call binding population: a
// unique-name call to it resolves like any other method (proving W4's virtual
// nodes are first-class in the resolver, not just the extractor).
func TestResolvePHP_PhpdocVirtualIsBindable(t *testing.T) {
	var s graph.Store = graph.New()
	f := "src/Foo.php"
	app := "src/a.php"
	phpType(s, f+"::Foo", "Foo", nil)
	s.AddNode(&graph.Node{ID: f + "::Foo.magicAccessor", Kind: graph.KindMethod, Name: "magicAccessor",
		FilePath: f, Language: "php", Meta: map[string]any{"receiver": "Foo", "virtual": "phpdoc_method"}})

	caller := app + "::run"
	s.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "run", FilePath: app, Language: "php"})
	s.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.magicAccessor", Kind: graph.EdgeCalls, FilePath: app, Line: 2})

	r := New(s)
	r.ResolveAll()

	var bound bool
	for _, e := range s.GetOutEdges(caller) {
		if e.Kind == graph.EdgeCalls && e.To == f+"::Foo.magicAccessor" {
			bound = true
		}
	}
	assert.True(t, bound, "a call to a unique phpdoc @method virtual must bind to it")
}

// Precision guard: same-name methods on UNRELATED types (no shared ancestor)
// must never be sprayed together.
func TestResolvePHPOverrideDispatch_UnrelatedNotFannedOut(t *testing.T) {
	var s graph.Store = graph.New()
	a := "src/Alpha.php"
	b := "src/Beta.php"
	app := "src/main.php"

	phpType(s, a+"::Alpha", "Alpha", nil)
	phpMethod(s, a+"::Alpha.save", "save", "Alpha")
	phpType(s, b+"::Beta", "Beta", nil)
	phpMethod(s, b+"::Beta.save", "save", "Beta")

	caller := app + "::go"
	s.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "go", FilePath: app, Language: "php"})
	s.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.save", Kind: graph.EdgeCalls, FilePath: app, Line: 3})

	r := New(s)
	r.resolvePHPOverrideDispatch()

	// Unrelated save() methods share no ancestor → the call stays ambiguous.
	var stillUnresolved bool
	for _, e := range s.GetOutEdges(caller) {
		if e.To == "unresolved::*.save" {
			stillUnresolved = true
		}
	}
	assert.True(t, stillUnresolved, "unrelated same-name methods must not be fanned out")
}

// Idempotency: a second pass over an already-bound graph must not duplicate
// the fan-out edges.
func TestResolvePHPOverrideDispatch_Idempotent(t *testing.T) {
	var s graph.Store = graph.New()
	iface := "src/I.php"
	x := "src/X.php"
	y := "src/Y.php"
	app := "src/a.php"

	phpIface(s, iface+"::I", "I", nil)
	phpMethod(s, iface+"::I.run", "run", "I")
	phpType(s, x+"::X", "X", map[string]any{"scope_interfaces": "I"})
	phpMethod(s, x+"::X.run", "run", "X")
	phpType(s, y+"::Y", "Y", map[string]any{"scope_interfaces": "I"})
	phpMethod(s, y+"::Y.run", "run", "Y")

	caller := app + "::z"
	s.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "z", FilePath: app, Language: "php"})
	s.AddEdge(&graph.Edge{From: caller, To: "unresolved::*.run", Kind: graph.EdgeCalls, FilePath: app, Line: 2})

	r := New(s)
	r.resolvePHPOverrideDispatch()
	countAfterFirst := len(s.GetOutEdges(caller))
	// Re-stub the primary to simulate a restub→re-resolve cycle.
	r.resolvePHPOverrideDispatch()
	assert.Equal(t, countAfterFirst, len(s.GetOutEdges(caller)),
		"re-running the pass must not duplicate fan-out edges")
}

// A typed receiver whose class does not declare the method binds to the
// nearest ancestor that does — the case a same-name match cannot see, because
// the declaring class is not the receiver's class. `$this->getRecord()` inside
// StreamHandlerTest resolves to TestCase.getRecord two levels up.
func TestResolvePHPOverrideDispatch_ReceiverTypeBindsInheritedMethod(t *testing.T) {
	var s graph.Store = graph.New()
	base := "tests/TestCase.php"
	mid := "tests/HandlerTestCase.php"
	leaf := "tests/StreamHandlerTest.php"

	phpType(s, base+"::TestCase", "TestCase", nil)
	phpMethod(s, base+"::TestCase.getRecord", "getRecord", "TestCase")
	phpType(s, mid+"::HandlerTestCase", "HandlerTestCase", map[string]any{MetaScopeParentClass: "TestCase"})
	phpType(s, leaf+"::StreamHandlerTest", "StreamHandlerTest", map[string]any{MetaScopeParentClass: "HandlerTestCase"})

	caller := leaf + "::StreamHandlerTest.testWrite"
	phpMethod(s, caller, "testWrite", "StreamHandlerTest")
	s.AddEdge(&graph.Edge{
		From: caller, To: "unresolved::*.getRecord", Kind: graph.EdgeCalls,
		FilePath: leaf, Line: 12,
		Meta: map[string]any{"receiver_type": "StreamHandlerTest"},
	})

	require.Positive(t, New(s).resolvePHPOverrideDispatch())

	out := s.GetOutEdges(caller)
	require.Len(t, out, 1)
	assert.Equal(t, base+"::TestCase.getRecord", out[0].To)
	assert.Equal(t, "receiver", out[0].Meta["dispatch"])
	assert.Equal(t, graph.OriginASTInferred, out[0].Origin)
}

// The receiver's own class wins over an ancestor that also declares the
// method — an override must not be attributed to the base it overrides.
func TestResolvePHPOverrideDispatch_ReceiverTypePrefersOwnDeclaration(t *testing.T) {
	var s graph.Store = graph.New()
	base := "src/AbstractHandler.php"
	leaf := "src/StreamHandler.php"
	app := "src/app.php"

	phpType(s, base+"::AbstractHandler", "AbstractHandler", nil)
	phpMethod(s, base+"::AbstractHandler.handle", "handle", "AbstractHandler")
	phpType(s, leaf+"::StreamHandler", "StreamHandler", map[string]any{MetaScopeParentClass: "AbstractHandler"})
	phpMethod(s, leaf+"::StreamHandler.handle", "handle", "StreamHandler")

	caller := app + "::run"
	s.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "run", FilePath: app, Language: "php"})
	s.AddEdge(&graph.Edge{
		From: caller, To: "unresolved::*.handle", Kind: graph.EdgeCalls,
		FilePath: app, Line: 3,
		Meta: map[string]any{"receiver_type": "StreamHandler"},
	})

	require.Positive(t, New(s).resolvePHPOverrideDispatch())

	out := s.GetOutEdges(caller)
	require.Len(t, out, 1, "a typed receiver binds once — it must not fan out")
	assert.Equal(t, leaf+"::StreamHandler.handle", out[0].To)
}

// A receiver typed as a class the graph does not know (a vendor dependency)
// stays unresolved: fanning it out across every same-named method would
// contradict the type the source states.
func TestResolvePHPOverrideDispatch_UnknownReceiverTypeStaysUnresolved(t *testing.T) {
	var s graph.Store = graph.New()
	a := "src/A.php"
	b := "src/B.php"
	app := "src/app.php"

	phpIface(s, a+"::Alpha", "Alpha", nil)
	phpMethod(s, a+"::Alpha.send", "send", "Alpha")
	phpType(s, b+"::Beta", "Beta", map[string]any{"scope_interfaces": "Alpha"})
	phpMethod(s, b+"::Beta.send", "send", "Beta")

	caller := app + "::run"
	s.AddNode(&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "run", FilePath: app, Language: "php"})
	s.AddEdge(&graph.Edge{
		From: caller, To: "unresolved::*.send", Kind: graph.EdgeCalls,
		FilePath: app, Line: 7,
		Meta: map[string]any{"receiver_type": "GuzzleClient"},
	})

	New(s).resolvePHPOverrideDispatch()

	out := s.GetOutEdges(caller)
	require.Len(t, out, 1)
	assert.Equal(t, "unresolved::*.send", out[0].To, "typed receiver must not fall back to a fan-out")
}

// A trait's method is reachable from the class that uses it: trait composition
// is an extends-shaped edge, so the ancestor walk crosses it.
func TestResolvePHPOverrideDispatch_ReceiverTypeBindsThroughTrait(t *testing.T) {
	var s graph.Store = graph.New()
	tr := "src/LogsMessages.php"
	cls := "src/Worker.php"

	phpType(s, tr+"::LogsMessages", "LogsMessages", nil)
	phpMethod(s, tr+"::LogsMessages.logInfo", "logInfo", "LogsMessages")
	phpType(s, cls+"::Worker", "Worker", nil)
	s.AddEdge(&graph.Edge{From: cls + "::Worker", To: tr + "::LogsMessages", Kind: graph.EdgeExtends, FilePath: cls, Line: 2})

	caller := cls + "::Worker.run"
	phpMethod(s, caller, "run", "Worker")
	s.AddEdge(&graph.Edge{
		From: caller, To: "unresolved::*.logInfo", Kind: graph.EdgeCalls,
		FilePath: cls, Line: 9,
		Meta: map[string]any{"receiver_type": "Worker"},
	})

	require.Positive(t, New(s).resolvePHPOverrideDispatch())
	assert.Equal(t, tr+"::LogsMessages.logInfo", s.GetOutEdges(caller)[0].To)
}

// A PHP "package" is one directory holding every sibling implementation, so the
// generic resolver's locality fallback — pick a same-directory method of the
// same name — is exactly wrong for an inherited call: `$this->getFormatter()`
// inside AmqpHandler landed on whichever sibling handler sorted first. When the
// receiver names a type this repo defines, the locality pick is withheld so the
// hierarchy walk can bind the declaration the receiver actually inherits.
func TestResolveAll_PHPTypedReceiverBeatsSameDirectoryGuess(t *testing.T) {
	var s graph.Store = graph.New()
	dir := "src/Handler/"
	iface := dir + "FormattableHandlerInterface.php"
	sibling := dir + "BufferHandler.php"
	caller := dir + "AmqpHandler.php"

	phpIface(s, iface+"::FormattableHandlerInterface", "FormattableHandlerInterface", nil)
	phpMethod(s, iface+"::FormattableHandlerInterface.getFormatter", "getFormatter", "FormattableHandlerInterface")
	// A same-directory sibling declaring the same name — the decoy the
	// locality fallback used to pick.
	phpType(s, sibling+"::BufferHandler", "BufferHandler", nil)
	phpMethod(s, sibling+"::BufferHandler.getFormatter", "getFormatter", "BufferHandler")

	phpType(s, caller+"::AmqpHandler", "AmqpHandler",
		map[string]any{"scope_interfaces": "FormattableHandlerInterface"})
	callerID := caller + "::AmqpHandler.handleBatch"
	phpMethod(s, callerID, "handleBatch", "AmqpHandler")
	s.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::*.getFormatter", Kind: graph.EdgeCalls,
		FilePath: caller, Line: 124,
		Meta: map[string]any{"receiver_type": "AmqpHandler"},
	})

	New(s).ResolveAll()

	out := s.GetOutEdges(callerID)
	require.Len(t, out, 1)
	assert.Equal(t, iface+"::FormattableHandlerInterface.getFormatter", out[0].To,
		"an inherited call must bind through the hierarchy, not to a same-directory sibling")
}

// The gate is limited to receivers this repo defines. A vendor-typed receiver
// has no hierarchy in the graph either, so withholding the locality pick would
// lose the edge with nowhere better to put it.
func TestResolveAll_PHPVendorTypedReceiverKeepsLocalityPick(t *testing.T) {
	var s graph.Store = graph.New()
	dir := "src/"
	local := dir + "Local.php"
	caller := dir + "Client.php"

	phpType(s, local+"::Local", "Local", nil)
	phpMethod(s, local+"::Local.send", "send", "Local")

	phpType(s, caller+"::Client", "Client", nil)
	callerID := caller + "::Client.run"
	phpMethod(s, callerID, "run", "Client")
	s.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::*.send", Kind: graph.EdgeCalls,
		FilePath: caller, Line: 9,
		// GuzzleClient is not defined anywhere in this graph.
		Meta: map[string]any{"receiver_type": "GuzzleClient"},
	})

	New(s).ResolveAll()

	out := s.GetOutEdges(callerID)
	require.Len(t, out, 1)
	assert.Equal(t, local+"::Local.send", out[0].To,
		"a vendor-typed receiver keeps the pre-existing locality behaviour")
}
