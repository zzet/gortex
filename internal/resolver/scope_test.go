package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// TestScope_CStaticPreference asserts that a C `static` function in
// the caller's file wins over a globally-linked function with the
// same name in another file. Without the scope rule, the generic
// locality cascade would still prefer the same-directory candidate;
// this test puts both candidates in the SAME directory so only the
// static-linkage stamp can disambiguate.
func TestScope_CStaticPreference(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.c", Kind: graph.KindFile, FilePath: "pkg/a.c", Language: "c"})
	g.AddNode(&graph.Node{ID: "pkg/b.c", Kind: graph.KindFile, FilePath: "pkg/b.c", Language: "c"})
	g.AddNode(&graph.Node{
		ID: "pkg/a.c::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "pkg/a.c", Language: "c",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/a.c::helper", Kind: graph.KindFunction, Name: "helper",
		FilePath: "pkg/a.c", Language: "c",
		Meta: map[string]any{MetaScopeStatic: true},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/b.c::helper", Kind: graph.KindFunction, Name: "helper",
		FilePath: "pkg/b.c", Language: "c",
	})
	e := &graph.Edge{
		From: "pkg/a.c::caller", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.c", Line: 5,
	}
	g.AddEdge(e)

	stats := New(g).ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/a.c::helper", e.To,
		"file-local static must win over a same-name candidate in another file")
}

// TestScope_CppSameNamespacePreference asserts a C++ function in the
// caller's namespace beats a same-named function in a different
// namespace, even when both live in the same directory.
func TestScope_CppSameNamespacePreference(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/a.cpp", Kind: graph.KindFile, FilePath: "src/a.cpp", Language: "cpp"})
	g.AddNode(&graph.Node{
		ID: "src/a.cpp::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "src/a.cpp", Language: "cpp",
		Meta: map[string]any{MetaScopeNamespace: "app"},
	})
	g.AddNode(&graph.Node{
		ID: "src/a.cpp::helper#app", Kind: graph.KindFunction, Name: "helper",
		FilePath: "src/a.cpp", Language: "cpp",
		Meta: map[string]any{MetaScopeNamespace: "app"},
	})
	g.AddNode(&graph.Node{
		ID: "src/a.cpp::helper#util", Kind: graph.KindFunction, Name: "helper",
		FilePath: "src/a.cpp", Language: "cpp",
		Meta: map[string]any{MetaScopeNamespace: "util"},
	})
	e := &graph.Edge{
		From: "src/a.cpp::caller", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "src/a.cpp", Line: 3,
	}
	g.AddEdge(e)

	stats := New(g).ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/a.cpp::helper#app", e.To,
		"same-namespace candidate must win over a same-directory cross-namespace one")
}

// TestScope_CppADLViaArgType asserts argument-dependent lookup: an
// unqualified call to `foo` whose argument hints at a type in
// namespace N must bind to a `foo` defined in namespace N, even when
// the caller's namespace has no such function.
func TestScope_CppADLViaArgType(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/a.cpp", Kind: graph.KindFile, FilePath: "src/a.cpp", Language: "cpp"})
	g.AddNode(&graph.Node{ID: "src/b.cpp", Kind: graph.KindFile, FilePath: "src/b.cpp", Language: "cpp"})
	g.AddNode(&graph.Node{
		ID: "src/a.cpp::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "src/a.cpp", Language: "cpp",
		Meta: map[string]any{MetaScopeNamespace: "app"},
	})
	// The only "process" candidate is in namespace `util` — same-
	// namespace lookup would miss it; ADL via the arg-type hint
	// (`util::Widget`) is the only path that lands the bind.
	g.AddNode(&graph.Node{
		ID: "src/b.cpp::process#util", Kind: graph.KindFunction, Name: "process",
		FilePath: "src/b.cpp", Language: "cpp",
		Meta: map[string]any{MetaScopeNamespace: "util"},
	})
	e := &graph.Edge{
		From: "src/a.cpp::caller", To: "unresolved::process",
		Kind: graph.EdgeCalls, FilePath: "src/a.cpp", Line: 4,
		Meta: map[string]any{MetaScopeArgTypes: "util::Widget"},
	}
	g.AddEdge(e)

	stats := New(g).ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/b.cpp::process#util", e.To,
		"ADL must walk arg-type namespaces when same-namespace lookup misses")
}

// TestScope_JavaEnclosingClassPreference asserts an unqualified call
// inside class X binds to X's method first, even when a same-named
// method exists on another class in the same package.
func TestScope_JavaEnclosingClassPreference(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "app/User.java", Kind: graph.KindFile, FilePath: "app/User.java", Language: "java"})
	g.AddNode(&graph.Node{
		ID: "app/User.java::User", Kind: graph.KindType, Name: "User",
		FilePath: "app/User.java", Language: "java",
	})
	g.AddNode(&graph.Node{
		ID: "app/User.java::User.save", Kind: graph.KindMethod, Name: "save",
		FilePath: "app/User.java", Language: "java",
		Meta: map[string]any{"receiver": "User", MetaScopeClass: "User"},
	})
	g.AddNode(&graph.Node{
		ID: "app/User.java::User.validate", Kind: graph.KindMethod, Name: "validate",
		FilePath: "app/User.java", Language: "java",
		Meta: map[string]any{"receiver": "User", MetaScopeClass: "User"},
	})
	g.AddNode(&graph.Node{
		ID: "app/Other.java::Other.validate", Kind: graph.KindMethod, Name: "validate",
		FilePath: "app/Other.java", Language: "java",
		Meta: map[string]any{"receiver": "Other", MetaScopeClass: "Other"},
	})
	// User.save() calls validate() unqualified — must bind to User.validate.
	e := &graph.Edge{
		From: "app/User.java::User.save", To: "unresolved::*.validate",
		Kind: graph.EdgeCalls, FilePath: "app/User.java", Line: 12,
	}
	g.AddEdge(e)

	stats := New(g).ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "app/User.java::User.validate", e.To,
		"unqualified call in class X must bind to X's method, not a same-named one elsewhere")
}

// TestScope_JavaSuperChainWalk asserts the scope resolver walks the
// extends chain when the unqualified call doesn't bind to the
// enclosing class.
func TestScope_JavaSuperChainWalk(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "app/Base.java", Kind: graph.KindFile, FilePath: "app/Base.java", Language: "java"})
	g.AddNode(&graph.Node{ID: "app/Child.java", Kind: graph.KindFile, FilePath: "app/Child.java", Language: "java"})
	g.AddNode(&graph.Node{
		ID: "app/Base.java::Base", Kind: graph.KindType, Name: "Base",
		FilePath: "app/Base.java", Language: "java",
	})
	g.AddNode(&graph.Node{
		ID: "app/Child.java::Child", Kind: graph.KindType, Name: "Child",
		FilePath: "app/Child.java", Language: "java",
		Meta: map[string]any{MetaScopeParentClass: "Base"},
	})
	g.AddNode(&graph.Node{
		ID: "app/Base.java::Base.helper", Kind: graph.KindMethod, Name: "helper",
		FilePath: "app/Base.java", Language: "java",
		Meta: map[string]any{"receiver": "Base", MetaScopeClass: "Base"},
	})
	g.AddNode(&graph.Node{
		ID: "app/Child.java::Child.run", Kind: graph.KindMethod, Name: "run",
		FilePath: "app/Child.java", Language: "java",
		Meta: map[string]any{"receiver": "Child", MetaScopeClass: "Child"},
	})
	// Decoy: another class has a same-name `helper`.
	g.AddNode(&graph.Node{
		ID: "app/Other.java::Other.helper", Kind: graph.KindMethod, Name: "helper",
		FilePath: "app/Other.java", Language: "java",
		Meta: map[string]any{"receiver": "Other", MetaScopeClass: "Other"},
	})
	// Child.run() calls helper() — should walk to Base.helper.
	e := &graph.Edge{
		From: "app/Child.java::Child.run", To: "unresolved::*.helper",
		Kind: graph.EdgeCalls, FilePath: "app/Child.java", Line: 4,
	}
	g.AddEdge(e)

	stats := New(g).ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "app/Base.java::Base.helper", e.To,
		"super-chain walk must land helper() on the parent class")
}

// TestScope_PhpParentCall asserts that `parent::foo()` in PHP binds
// to the parent class's foo, even with a same-name decoy elsewhere.
func TestScope_PhpParentCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/Base.php", Kind: graph.KindFile, FilePath: "src/Base.php", Language: "php"})
	g.AddNode(&graph.Node{ID: "src/Child.php", Kind: graph.KindFile, FilePath: "src/Child.php", Language: "php"})
	g.AddNode(&graph.Node{
		ID: "src/Base.php::Base", Kind: graph.KindType, Name: "Base",
		FilePath: "src/Base.php", Language: "php",
	})
	g.AddNode(&graph.Node{
		ID: "src/Child.php::Child", Kind: graph.KindType, Name: "Child",
		FilePath: "src/Child.php", Language: "php",
		Meta: map[string]any{MetaScopeParentClass: "Base"},
	})
	g.AddNode(&graph.Node{
		ID: "src/Base.php::Base.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "src/Base.php", Language: "php",
		Meta: map[string]any{"receiver": "Base", MetaScopeClass: "Base"},
	})
	g.AddNode(&graph.Node{
		ID: "src/Child.php::Child.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "src/Child.php", Language: "php",
		Meta: map[string]any{"receiver": "Child", MetaScopeClass: "Child"},
	})
	g.AddNode(&graph.Node{
		ID: "src/Other.php::Other.handle", Kind: graph.KindMethod, Name: "handle",
		FilePath: "src/Other.php", Language: "php",
		Meta: map[string]any{"receiver": "Other", MetaScopeClass: "Other"},
	})
	// Child.handle() calls parent::handle() — must bind to Base.handle.
	g.AddEdge(&graph.Edge{
		From: "src/Child.php::Child", To: "src/Base.php::Base",
		Kind: graph.EdgeExtends, FilePath: "src/Child.php",
		Meta: map[string]any{"target_fqn": "Base"},
	})
	e := &graph.Edge{
		From: "src/Child.php::Child.handle", To: "unresolved::*.handle",
		Kind: graph.EdgeCalls, FilePath: "src/Child.php", Line: 7,
		Meta: map[string]any{MetaScopeKind: ScopeKindParent},
	}
	g.AddEdge(e)

	stats := New(g).ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/Base.php::Base.handle", e.To,
		"parent:: must walk extends chain to Base.handle")
}

// TestScope_PhpSelfCall asserts `self::foo()` pins to the enclosing
// class's method, regardless of where the same-named decoy lives.
func TestScope_PhpSelfCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/Service.php", Kind: graph.KindFile, FilePath: "src/Service.php", Language: "php"})
	g.AddNode(&graph.Node{
		ID: "src/Service.php::Service.boot", Kind: graph.KindMethod, Name: "boot",
		FilePath: "src/Service.php", Language: "php",
		Meta: map[string]any{"receiver": "Service", MetaScopeClass: "Service"},
	})
	g.AddNode(&graph.Node{
		ID: "src/Service.php::Service.init", Kind: graph.KindMethod, Name: "init",
		FilePath: "src/Service.php", Language: "php",
		Meta: map[string]any{"receiver": "Service", MetaScopeClass: "Service"},
	})
	g.AddNode(&graph.Node{
		ID: "src/Other.php::Other.init", Kind: graph.KindMethod, Name: "init",
		FilePath: "src/Other.php", Language: "php",
		Meta: map[string]any{"receiver": "Other", MetaScopeClass: "Other"},
	})
	e := &graph.Edge{
		From: "src/Service.php::Service.boot", To: "unresolved::*.init",
		Kind: graph.EdgeCalls, FilePath: "src/Service.php", Line: 3,
		Meta: map[string]any{MetaScopeKind: ScopeKindSelf},
	}
	g.AddEdge(e)

	stats := New(g).ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "src/Service.php::Service.init", e.To,
		"self:: must pin to the enclosing class's method")
}

// TestScope_StampedAsScopeResolution asserts the scope path stamps
// `resolution=scope` on the edge so the gain telemetry + agent
// observability can tell scope wins from generic-locality wins.
func TestScope_StampedAsScopeResolution(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.c", Kind: graph.KindFile, FilePath: "pkg/a.c", Language: "c"})
	g.AddNode(&graph.Node{
		ID: "pkg/a.c::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "pkg/a.c", Language: "c",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/a.c::helper", Kind: graph.KindFunction, Name: "helper",
		FilePath: "pkg/a.c", Language: "c",
		Meta: map[string]any{MetaScopeStatic: true},
	})
	g.AddNode(&graph.Node{
		ID: "pkg/b.c::helper", Kind: graph.KindFunction, Name: "helper",
		FilePath: "pkg/b.c", Language: "c",
	})
	e := &graph.Edge{
		From: "pkg/a.c::caller", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.c", Line: 5,
	}
	g.AddEdge(e)

	New(g).ResolveAll()

	require.NotNil(t, e.Meta)
	assert.Equal(t, "scope", e.Meta["resolution"],
		"scope-resolved edge must carry resolution=scope so telemetry can distinguish it")
	assert.Equal(t, graph.OriginASTResolved, e.Origin,
		"scope-resolved edge must stamp OriginASTResolved")
}

// TestScope_FallthroughWhenNoEvidence asserts the scope resolver is a
// no-op when none of the language-specific evidence is present —
// behaviour stays identical to pre-N8 for unaffected call sites.
func TestScope_FallthroughWhenNoEvidence(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.c", Kind: graph.KindFile, FilePath: "pkg/a.c", Language: "c"})
	g.AddNode(&graph.Node{
		ID: "pkg/a.c::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "pkg/a.c", Language: "c",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/a.c::helper", Kind: graph.KindFunction, Name: "helper",
		FilePath: "pkg/a.c", Language: "c",
		// No scope_static, no scope_ns, no scope_class.
	})
	e := &graph.Edge{
		From: "pkg/a.c::caller", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "pkg/a.c", Line: 5,
	}
	g.AddEdge(e)

	stats := New(g).ResolveAll()

	require.Equal(t, 1, stats.Resolved)
	assert.Equal(t, "pkg/a.c::helper", e.To,
		"no-evidence edge must still resolve via the generic cascade")
	// Without scope evidence, resolution should NOT be tagged
	// "scope" — the generic locality path took over.
	if e.Meta != nil {
		assert.NotEqual(t, "scope", e.Meta["resolution"],
			"generic-locality resolution must not falsely claim scope evidence")
	}
}
