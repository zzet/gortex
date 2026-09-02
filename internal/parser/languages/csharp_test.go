package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Regression: a bodyless interface-method signature must mint a
// <file>::<Iface>.<name> KindMethod node (marked iface_member) so its id
// resolves and dispatch calls bind to it — parity with PHP's extractMethod.
func TestCSharpExtractor_BodylessInterfaceMethodNode(t *testing.T) {
	src := []byte(`namespace App {
    public interface ISink {
        void Write(string msg);
        string Flush();
    }
    public class FileSink : ISink {
        public void Write(string msg) {}
        public string Flush() { return ""; }
    }
}`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Sink.cs", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	m := byID["Sink.cs::ISink.Write"]
	require.NotNil(t, m, "bodyless interface method must mint a node")
	assert.Equal(t, graph.KindMethod, m.Kind)
	assert.Equal(t, "ISink", m.Meta["receiver"])
	assert.Equal(t, true, m.Meta["iface_member"])

	require.NotNil(t, byID["Sink.cs::ISink.Flush"], "every bodyless interface method mints a node")
}

func TestCSharpExtractor_ClassWithMethods(t *testing.T) {
	src := []byte(`public class UserService {
    public User FindById(string id) {
        return null;
    }

    public void Save(User user) {
        _db.Execute(user);
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("UserService.cs", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "UserService", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	assert.Equal(t, "FindById", methods[0].Name)
	assert.Equal(t, "Save", methods[1].Name)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 2)
	for _, e := range memberEdges {
		assert.Equal(t, "UserService.cs::UserService", e.To)
	}
}

func TestCSharpExtractor_Interface(t *testing.T) {
	src := []byte(`public interface IUserService {
    User FindById(string id);
    void Save(User user);
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("IUserService.cs", src)
	require.NoError(t, err)

	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "IUserService", ifaces[0].Name)
	require.NotNil(t, ifaces[0].Meta)
	methods, ok := ifaces[0].Meta["methods"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{"FindById", "Save"}, methods)
}

// TestCSharpExtractor_InterfaceMembers verifies that interface member
// declarations — overloaded methods, a property, and a C# 8 default (bodied)
// method — each get their own <file>::<Iface>.<member> node marked
// iface_member, while the interface node still carries its method-name list.
func TestCSharpExtractor_InterfaceMembers(t *testing.T) {
	src := []byte(`public interface ITruncator {
    string Truncate(string value);
    string Truncate(string value, int length);
    int Count { get; }
    string Describe() => "default";
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("ITruncator.cs", src)
	require.NoError(t, err)

	// First Truncate overload keeps the bare id; the second gets the
	// _L<line> suffix (line 3 in the source above).
	t1 := nodeByID(result.Nodes, "ITruncator.cs::ITruncator.Truncate")
	require.NotNil(t, t1, "first Truncate overload node")
	assert.Equal(t, graph.KindMethod, t1.Kind)
	assert.Equal(t, "ITruncator", t1.Meta["receiver"])
	assert.Equal(t, VisibilityPublic, t1.Meta["visibility"])
	assert.Equal(t, true, t1.Meta["iface_member"])

	t2 := nodeByID(result.Nodes, "ITruncator.cs::ITruncator.Truncate_L3")
	require.NotNil(t, t2, "overloaded Truncate gets the _L<line> id")
	assert.Equal(t, true, t2.Meta["iface_member"])

	// Property member — a KindField node marked property + iface_member.
	count := nodeByID(result.Nodes, "ITruncator.cs::ITruncator.Count")
	require.NotNil(t, count, "interface property node")
	assert.Equal(t, graph.KindField, count.Kind)
	assert.Equal(t, "property", count.Meta["kind"])
	assert.Equal(t, true, count.Meta["iface_member"])

	// C# 8 default (bodied) method — must NOT leak as a bare function and
	// is still marked an interface member.
	describe := nodeByID(result.Nodes, "ITruncator.cs::ITruncator.Describe")
	require.NotNil(t, describe, "default interface method node")
	assert.Equal(t, graph.KindMethod, describe.Kind)
	assert.Equal(t, true, describe.Meta["iface_member"])
	assert.Nil(t, nodeByID(result.Nodes, "ITruncator.cs::Describe"),
		"default method must not leak as a bare function")

	// Each member is a MemberOf the interface type node.
	memberOf := 0
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeMemberOf && ed.To == "ITruncator.cs::ITruncator" {
			memberOf++
		}
	}
	assert.Equal(t, 4, memberOf, "2 method overloads + property + default method")

	// Backward compat: the interface node still lists its method names.
	iface := nodeByID(result.Nodes, "ITruncator.cs::ITruncator")
	require.NotNil(t, iface)
	names, _ := iface.Meta["methods"].([]string)
	assert.Contains(t, names, "Truncate")
	assert.Contains(t, names, "Describe")
}

// TestCSharpExtractor_ExtensionMethod verifies extension methods (a static
// method whose first parameter carries the `this` modifier) are stamped with
// extension=true + this_param_type, keep their <StaticClass>.<name> id, and a
// plain static method is not misflagged.
func TestCSharpExtractor_ExtensionMethod(t *testing.T) {
	src := []byte(`public static class Exts {
    public static string Dehumanize(this string value) { return value; }
    public static int AddTo(this int x, int y) { return x + y; }
    public static string Plain(string value) { return value; }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Exts.cs", src)
	require.NoError(t, err)

	deh := nodeByID(result.Nodes, "Exts.cs::Exts.Dehumanize")
	require.NotNil(t, deh, "extension method id stays <StaticClass>.<name>")
	assert.Equal(t, true, deh.Meta["extension"])
	assert.Equal(t, "string", deh.Meta["this_param_type"])
	assert.Equal(t, true, deh.Meta["static"])

	add := nodeByID(result.Nodes, "Exts.cs::Exts.AddTo")
	require.NotNil(t, add)
	assert.Equal(t, true, add.Meta["extension"])
	assert.Equal(t, "int", add.Meta["this_param_type"])

	// A plain static method (no `this`) must not be flagged an extension.
	plain := nodeByID(result.Nodes, "Exts.cs::Exts.Plain")
	require.NotNil(t, plain)
	_, isExt := plain.Meta["extension"]
	assert.False(t, isExt, "plain static method must not be an extension")
}

// csharpSymbolNames returns the set of non-file node names in a result.
func csharpSymbolNames(res *parser.ExtractionResult) map[string]bool {
	names := map[string]bool{}
	for _, n := range res.Nodes {
		if n != nil && n.Kind != graph.KindFile {
			names[n.Name] = true
		}
	}
	return names
}

// TestCSharpConditionalRecoversSymbolsViaAdaptiveReparse is a B1 named test:
// when a conditional directive desynchronises the native parse (here a #if that
// brackets a stray brace plus a whole namespace), the adaptive re-parse falls
// back to the directive-blanked source and recovers the symbols the native
// parse dropped — without the caller doing anything.
func TestCSharpConditionalRecoversSymbolsViaAdaptiveReparse(t *testing.T) {
	src := []byte("public class C {\n" +
		"    public void A() {}\n" +
		"#if NET\n" +
		"}\n" +
		"namespace Extra {\n" +
		"    public class D { public void B() {} }\n" +
		"}\n" +
		"#endif\n")
	res, err := NewCSharpExtractor().Extract("R.cs", src)
	require.NoError(t, err)

	names := csharpSymbolNames(res)
	// The native parse keeps only C and A; the blanked re-parse additionally
	// recovers the #if-guarded namespace's class D and its method B.
	assert.True(t, names["D"], "class D should be recovered by the adaptive re-parse; got %v", names)
	assert.True(t, names["B"], "method B should be recovered by the adaptive re-parse; got %v", names)
	assert.True(t, names["A"], "method A must still be present; got %v", names)
}

// TestCSharpConditionalKeepsAllBranchMethods proves the native tree-sitter
// handling — which already extracts methods from every #if/#elif/#else branch —
// is preserved (the adaptive path only ever adds symbols, never drops them).
func TestCSharpConditionalKeepsAllBranchMethods(t *testing.T) {
	src := []byte("public class C {\n" +
		"#if A\n" +
		"    public void M_A() {}\n" +
		"#elif B\n" +
		"    public void M_B() {}\n" +
		"#else\n" +
		"    public void M_C() {}\n" +
		"#endif\n" +
		"    public void Always() {}\n" +
		"}\n")
	res, err := NewCSharpExtractor().Extract("R.cs", src)
	require.NoError(t, err)

	names := csharpSymbolNames(res)
	for _, want := range []string{"M_A", "M_B", "M_C", "Always"} {
		assert.True(t, names[want], "%s should be extracted from its branch; got %v", want, names)
	}
}

func TestCSharpExtractor_UsingImports(t *testing.T) {
	src := []byte(`using System;
using System.Collections.Generic;

public class App {}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 2)
}

func TestCSharpExtractor_Namespace(t *testing.T) {
	src := []byte(`namespace MyApp.Services
{
    public class Foo {}
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Foo.cs", src)
	require.NoError(t, err)

	pkgs := nodesOfKind(result.Nodes, graph.KindPackage)
	require.Len(t, pkgs, 1)
	assert.Equal(t, "MyApp.Services", pkgs[0].Name)
}

func TestCSharpExtractor_StructAndEnum(t *testing.T) {
	src := []byte(`public enum Status {
    Active,
    Inactive
}

public struct Point {
    public int X;
    public int Y;
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Types.cs", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 2)
	names := []string{types[0].Name, types[1].Name}
	assert.Contains(t, names, "Status")
	assert.Contains(t, names, "Point")

	// Struct fields should be extracted.
	fields := nodesOfKind(result.Nodes, graph.KindField)
	assert.Len(t, fields, 2)

	// Enum members are navigable nodes with their own MemberOf edges.
	members := nodesOfKind(result.Nodes, graph.KindEnumMember)
	assert.Len(t, members, 2, "Active + Inactive")

	pointMembers, statusMembers := 0, 0
	for _, e := range edgesOfKind(result.Edges, graph.EdgeMemberOf) {
		switch e.To {
		case "Types.cs::Point":
			pointMembers++
		case "Types.cs::Status":
			statusMembers++
		}
	}
	assert.Equal(t, 2, pointMembers, "struct fields belong to Point")
	assert.Equal(t, 2, statusMembers, "enum members belong to Status")
}

func TestCSharpExtractor_Constructor(t *testing.T) {
	src := []byte(`public class UserService {
    private readonly Database _db;

    public UserService(Database db) {
        _db = db;
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("UserService.cs", src)
	require.NoError(t, err)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "UserService.<init>", methods[0].Name)

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	// Constructor + field = 2 MemberOf edges
	require.GreaterOrEqual(t, len(memberEdges), 1)
	found := false
	for _, e := range memberEdges {
		if e.From == "UserService.cs::UserService.<init>" {
			assert.Equal(t, "UserService.cs::UserService", e.To)
			found = true
		}
	}
	assert.True(t, found, "constructor should have MemberOf edge to class")
}

func TestCSharpExtractor_FullSample(t *testing.T) {
	src := []byte(`using System;
using System.Collections.Generic;

namespace MyApp.Services
{
    public interface IUserService
    {
        User FindById(string id);
        void Save(User user);
    }

    public class UserService : IUserService
    {
        private readonly Database _db;

        public UserService(Database db)
        {
            _db = db;
        }

        public User FindById(string id)
        {
            return _db.Query(id);
        }

        public void Save(User user)
        {
            _db.Execute(user);
        }
    }

    public enum Status
    {
        Active,
        Inactive
    }

    public struct Point
    {
        public int X;
        public int Y;
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Services.cs", src)
	require.NoError(t, err)

	// 1 namespace
	pkgs := nodesOfKind(result.Nodes, graph.KindPackage)
	assert.Len(t, pkgs, 1)

	// 1 interface
	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	assert.Len(t, ifaces, 1)

	// 3 types: UserService, Status, Point
	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.Len(t, types, 3)

	// 5 methods: the 2 interface members (IUserService.FindById / .Save)
	// plus the concrete UserService constructor + FindById + Save.
	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.Len(t, methods, 5)

	// 2 imports
	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.Len(t, imports, 2)

	// Call edges (Query, Execute)
	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 2)
}

func TestCSharpExtractor_TypeEnv_ExplicitType(t *testing.T) {
	src := []byte(`public class UserService {
    public void Save() {}
}

public class App {
    public void Main() {
        UserService svc = new UserService();
        svc.Save();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("app.cs", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var saveCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Save") {
			saveCall = c
			break
		}
	}
	require.NotNil(t, saveCall, "expected a call edge to Save")
	require.NotNil(t, saveCall.Meta, "expected Meta on Save call edge")
	assert.Equal(t, "UserService", saveCall.Meta["receiver_type"])
}

func TestCSharpExtractor_TypeEnv_NewExpression(t *testing.T) {
	src := []byte(`public class Client {
    public void Connect() {}
}

public class App {
    public void Main() {
        var client = new Client();
        client.Connect();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("app.cs", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var connectCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Connect") {
			connectCall = c
			break
		}
	}
	require.NotNil(t, connectCall)
	require.NotNil(t, connectCall.Meta)
	assert.Equal(t, "Client", connectCall.Meta["receiver_type"])
}

func TestCSharpExtractor_TypeEnv_Unknown(t *testing.T) {
	src := []byte(`public class App {
    public object GetService() { return null; }

    public void Main() {
        var svc = GetService();
        svc.Process();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("app.cs", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var processCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "Process") {
			processCall = c
			break
		}
	}
	require.NotNil(t, processCall)
	assert.NotContains(t, processCall.Meta, "receiver_type", "unknown type should not produce a receiver_type hint")
}

func TestCSharpExtractor_TypeEnv_Chain(t *testing.T) {
	src := []byte(`public class Order {
    public int Id;
}

public class UserService {
    public Order GetOrder() { return new Order(); }
}

public class App {
    public void Main() {
        UserService svc = new UserService();
        svc.GetOrder().ToString();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("app.cs", src)
	require.NoError(t, err)

	// Verify return_type is set on GetOrder method.
	var getOrderNode *graph.Node
	for _, n := range result.Nodes {
		if n.Name == "GetOrder" {
			getOrderNode = n
			break
		}
	}
	require.NotNil(t, getOrderNode, "expected a node for GetOrder")
	assert.Equal(t, "Order", getOrderNode.Meta["return_type"])

	// Verify chain resolution: svc.GetOrder() should resolve to Order.
	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	var toStringCall *graph.Edge
	for _, c := range calls {
		if strings.HasSuffix(c.To, "ToString") {
			toStringCall = c
			break
		}
	}
	require.NotNil(t, toStringCall, "expected a call edge to ToString")
	require.NotNil(t, toStringCall.Meta, "expected Meta on ToString call edge")
	assert.Equal(t, "Order", toStringCall.Meta["receiver_type"])
}

func TestCSharpExtractor_DocAndVisibility(t *testing.T) {
	src := []byte(`namespace X
{
    /// <summary>
    /// Greeter wraps the greeting.
    /// </summary>
    public class Greeter
    {
        /// <summary>Says hi.</summary>
        public void Hello() {}

        private void Secret() {}
    }

    class Internal {}
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Greeter.cs", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	greeter := byID["Greeter.cs::Greeter"]
	require.NotNil(t, greeter)
	if greeter.Meta["visibility"] != "public" {
		t.Fatalf("Greeter.vis = %q", greeter.Meta["visibility"])
	}
	if greeter.Meta["doc"] != "Greeter wraps the greeting." {
		t.Fatalf("Greeter.doc = %q", greeter.Meta["doc"])
	}

	hello := byID["Greeter.cs::Greeter.Hello"]
	require.NotNil(t, hello)
	if hello.Meta["visibility"] != "public" {
		t.Fatalf("Hello.vis = %q", hello.Meta["visibility"])
	}
	if hello.Meta["doc"] != "Says hi." {
		t.Fatalf("Hello.doc = %q", hello.Meta["doc"])
	}

	secret := byID["Greeter.cs::Greeter.Secret"]
	require.NotNil(t, secret)
	if secret.Meta["visibility"] != "private" {
		t.Fatalf("Secret.vis = %q", secret.Meta["visibility"])
	}

	internalT := byID["Greeter.cs::Internal"]
	require.NotNil(t, internalT)
	if internalT.Meta["visibility"] != "internal" {
		t.Fatalf("Internal.vis = %q", internalT.Meta["visibility"])
	}
}

// edgeTargetNames returns the bare target names of every edge of the
// given kind whose From matches the given source ID. The C# base-list
// heuristic emits unresolved targets (`unresolved::Name`), so the
// prefix is stripped for readable assertions.
func edgeTargetNames(edges []*graph.Edge, from string, kind graph.EdgeKind) []string {
	var out []string
	for _, e := range edges {
		if e.Kind != kind || e.From != from {
			continue
		}
		out = append(out, strings.TrimPrefix(e.To, "unresolved::"))
	}
	return out
}

func TestCSharpExtractor_BaseListDiscrimination(t *testing.T) {
	e := NewCSharpExtractor()

	t.Run("class with base class and interface", func(t *testing.T) {
		src := []byte(`class Foo : BaseClass, IService {}`)
		result, err := e.Extract("Foo.cs", src)
		require.NoError(t, err)

		extends := edgeTargetNames(result.Edges, "Foo.cs::Foo", graph.EdgeExtends)
		implements := edgeTargetNames(result.Edges, "Foo.cs::Foo", graph.EdgeImplements)
		assert.Equal(t, []string{"BaseClass"}, extends)
		assert.Equal(t, []string{"IService"}, implements)

		// Heuristic edges ride at the inferred tier, not resolved.
		for _, ed := range result.Edges {
			if ed.Kind == graph.EdgeExtends || ed.Kind == graph.EdgeImplements {
				assert.Equal(t, graph.OriginASTInferred, ed.Origin)
			}
		}
	})

	t.Run("base resolved via local interface prescan", func(t *testing.T) {
		// IThing breaks the I-prefix convention (it does, but we also
		// confirm a same-file interface is honoured even by name): the
		// prescan must classify Bar's base as an interface.
		src := []byte(`interface IThing {}
class Bar : IThing {}`)
		result, err := e.Extract("Bar.cs", src)
		require.NoError(t, err)

		assert.Empty(t, edgeTargetNames(result.Edges, "Bar.cs::Bar", graph.EdgeExtends))
		assert.Equal(t, []string{"IThing"},
			edgeTargetNames(result.Edges, "Bar.cs::Bar", graph.EdgeImplements))
	})

	t.Run("prescan wins over name shape", func(t *testing.T) {
		// Widget does not look like an interface (no I-prefix) but is
		// declared as one in this file — the prescan must win, so it is
		// implemented, not extended.
		src := []byte(`interface Widget {}
class Panel : Widget {}`)
		result, err := e.Extract("Panel.cs", src)
		require.NoError(t, err)

		assert.Empty(t, edgeTargetNames(result.Edges, "Panel.cs::Panel", graph.EdgeExtends))
		assert.Equal(t, []string{"Widget"},
			edgeTargetNames(result.Edges, "Panel.cs::Panel", graph.EdgeImplements))
	})

	t.Run("struct implements only, never extends", func(t *testing.T) {
		src := []byte(`struct S : IComparable {}`)
		result, err := e.Extract("S.cs", src)
		require.NoError(t, err)

		assert.Empty(t, edgeTargetNames(result.Edges, "S.cs::S", graph.EdgeExtends))
		assert.Equal(t, []string{"IComparable"},
			edgeTargetNames(result.Edges, "S.cs::S", graph.EdgeImplements))
	})

	t.Run("generic interface strips type arguments", func(t *testing.T) {
		src := []byte(`class L : IList<int> {}`)
		result, err := e.Extract("L.cs", src)
		require.NoError(t, err)

		assert.Empty(t, edgeTargetNames(result.Edges, "L.cs::L", graph.EdgeExtends))
		assert.Equal(t, []string{"IList"},
			edgeTargetNames(result.Edges, "L.cs::L", graph.EdgeImplements))
	})

	t.Run("generic base class extends with stripped name", func(t *testing.T) {
		src := []byte(`class C : Base<int>, IList<int> {}`)
		result, err := e.Extract("C.cs", src)
		require.NoError(t, err)

		assert.Equal(t, []string{"Base"},
			edgeTargetNames(result.Edges, "C.cs::C", graph.EdgeExtends))
		assert.Equal(t, []string{"IList"},
			edgeTargetNames(result.Edges, "C.cs::C", graph.EdgeImplements))
	})

	t.Run("qualified base name reduced to simple name", func(t *testing.T) {
		src := []byte(`class Outer : System.Object, ICloneable {}`)
		result, err := e.Extract("Outer.cs", src)
		require.NoError(t, err)

		assert.Equal(t, []string{"Object"},
			edgeTargetNames(result.Edges, "Outer.cs::Outer", graph.EdgeExtends))
		assert.Equal(t, []string{"ICloneable"},
			edgeTargetNames(result.Edges, "Outer.cs::Outer", graph.EdgeImplements))
	})

	// The two cases above cross here: a qualified name whose FINAL
	// segment is itself generic. That segment parses as a `generic_name`,
	// not an `identifier`, so scanning a qualified_name's direct
	// identifier children walked straight past it and returned the
	// penultimate segment - the namespace - as the base's name.
	t.Run("qualified generic base reduces to its final segment", func(t *testing.T) {
		src := []byte(`class Dual : App.Base<int>, App.IBox<int> {}`)
		result, err := e.Extract("Dual.cs", src)
		require.NoError(t, err)

		assert.Equal(t, []string{"Base"},
			edgeTargetNames(result.Edges, "Dual.cs::Dual", graph.EdgeExtends))
		assert.Equal(t, []string{"IBox"},
			edgeTargetNames(result.Edges, "Dual.cs::Dual", graph.EdgeImplements))
	})

	t.Run("record extends base and implements interface", func(t *testing.T) {
		src := []byte(`record Rec(int X) : Base(X), IThing {}`)
		result, err := e.Extract("Rec.cs", src)
		require.NoError(t, err)

		// The primary_constructor_base_type Base(X) is always a base class.
		assert.Equal(t, []string{"Base"},
			edgeTargetNames(result.Edges, "Rec.cs::Rec", graph.EdgeExtends))
		assert.Equal(t, []string{"IThing"},
			edgeTargetNames(result.Edges, "Rec.cs::Rec", graph.EdgeImplements))
	})

	t.Run("record struct implements only", func(t *testing.T) {
		src := []byte(`record struct RS : IFoo {}`)
		result, err := e.Extract("RS.cs", src)
		require.NoError(t, err)

		assert.Empty(t, edgeTargetNames(result.Edges, "RS.cs::RS", graph.EdgeExtends))
		assert.Equal(t, []string{"IFoo"},
			edgeTargetNames(result.Edges, "RS.cs::RS", graph.EdgeImplements))
	})
}

// TestCSharpExtractor_InterfaceBaseList: an interface declaration's
// base list is interface inheritance — every entry must emit
// EdgeExtends at the same inferred tier as the class path. Without
// these edges a derived interface has no hierarchy in the base graph.
func TestCSharpExtractor_InterfaceBaseList(t *testing.T) {
	e := NewCSharpExtractor()

	t.Run("single base interface", func(t *testing.T) {
		src := []byte(`interface IDerived : IBase {}`)
		result, err := e.Extract("D.cs", src)
		require.NoError(t, err)

		assert.Equal(t, []string{"IBase"},
			edgeTargetNames(result.Edges, "D.cs::IDerived", graph.EdgeExtends))
		assert.Empty(t, edgeTargetNames(result.Edges, "D.cs::IDerived", graph.EdgeImplements))
	})

	t.Run("generic base strips type arguments", func(t *testing.T) {
		// Type params on the declaring interface and a where-clause
		// (a base_list sibling in the grammar) must not disturb the
		// entry walk.
		src := []byte(`interface ILedger<T> : IReadRepository<T> where T : class {}`)
		result, err := e.Extract("L.cs", src)
		require.NoError(t, err)

		assert.Equal(t, []string{"IReadRepository"},
			edgeTargetNames(result.Edges, "L.cs::ILedger", graph.EdgeExtends))
	})

	t.Run("qualified base keeps target_fqn for namespace narrowing", func(t *testing.T) {
		src := []byte(`interface IHandle : System.IDisposable {}`)
		result, err := e.Extract("H.cs", src)
		require.NoError(t, err)

		assert.Equal(t, []string{"IDisposable"},
			edgeTargetNames(result.Edges, "H.cs::IHandle", graph.EdgeExtends))
		for _, ed := range result.Edges {
			if ed.From == "H.cs::IHandle" && ed.Kind == graph.EdgeExtends {
				assert.Equal(t, "System.IDisposable", ed.Meta["target_fqn"])
			}
		}
	})

	t.Run("multiple bases all extend", func(t *testing.T) {
		src := []byte(`interface IBoth : IReader, IWriter {}`)
		result, err := e.Extract("B.cs", src)
		require.NoError(t, err)

		assert.Equal(t, []string{"IReader", "IWriter"},
			edgeTargetNames(result.Edges, "B.cs::IBoth", graph.EdgeExtends))
	})

	t.Run("inferred tier and no edges without a base list", func(t *testing.T) {
		src := []byte(`interface IDerived : IBase {}
interface IPlain {}`)
		result, err := e.Extract("T.cs", src)
		require.NoError(t, err)

		found := false
		for _, ed := range result.Edges {
			if ed.From == "T.cs::IDerived" && ed.Kind == graph.EdgeExtends {
				found = true
				assert.Equal(t, graph.OriginASTInferred, ed.Origin)
			}
		}
		assert.True(t, found, "IDerived must emit an extends edge")
		assert.Empty(t, edgeTargetNames(result.Edges, "T.cs::IPlain", graph.EdgeExtends))
		assert.Empty(t, edgeTargetNames(result.Edges, "T.cs::IPlain", graph.EdgeImplements))
	})
}

// TestCSharpEnumMembersConstsAndFlags is the C8 test: enum members extract as
// navigable nodes (with values), `const` fields classify as constants,
// async/static/readonly/value-type flags are stamped, and types/methods carry
// their namespace scope.
func TestCSharpEnumMembersConstsAndFlags(t *testing.T) {
	src := []byte("namespace App.Core {\n" +
		"  public enum Color { Red, Green = 5, Blue }\n" +
		"  public struct Point { public int X; }\n" +
		"  public class C {\n" +
		"    public const int MAX = 10;\n" +
		"    private static readonly int Y = 1;\n" +
		"    public async Task<int> FetchAsync() { return 1; }\n" +
		"    public static void Helper() {}\n" +
		"  }\n}\n")
	res, err := NewCSharpExtractor().Extract("a.cs", src)
	require.NoError(t, err)

	byName := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byName[n.Name] = n
	}

	// Enum members.
	for _, m := range []string{"Red", "Green", "Blue"} {
		require.NotNil(t, byName[m], "enum member %s should be a node", m)
		assert.Equal(t, graph.KindEnumMember, byName[m].Kind)
	}
	assert.Equal(t, "5", byName["Green"].Meta["value"], "explicit enum value")

	// const → constant; flags.
	require.NotNil(t, byName["MAX"])
	assert.Equal(t, graph.KindConstant, byName["MAX"].Kind, "const field classifies as a constant")
	assert.Equal(t, true, byName["Y"].Meta["static"])
	assert.Equal(t, true, byName["Y"].Meta["readonly"])
	assert.Equal(t, true, byName["FetchAsync"].Meta["async"])
	assert.Equal(t, true, byName["Helper"].Meta["static"])

	// value type + namespace scope.
	assert.Equal(t, true, byName["Point"].Meta["value_type"], "struct is a value type")
	assert.Equal(t, "App.Core", byName["C"].Meta["scope_ns"])
	assert.Equal(t, "App.Core", byName["FetchAsync"].Meta["scope_ns"])
}

func TestCSharpTypeFlavor(t *testing.T) {
	src := []byte(`namespace App;

class Service {}
struct Vec { public int X; }
interface IStore {}
enum Color { Red }
record Rec(int X);
record struct RVec(int X);
`)
	res, err := NewCSharpExtractor().Extract("flavor.cs", src)
	require.NoError(t, err)

	byName := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byName[n.Name] = n
	}

	require.NotNil(t, byName["Service"])
	assert.Equal(t, "class", byName["Service"].Meta["type_flavor"])

	require.NotNil(t, byName["Vec"])
	assert.Equal(t, "struct", byName["Vec"].Meta["type_flavor"])
	// Dual-write: the legacy value_type marker stays beside type_flavor.
	assert.Equal(t, true, byName["Vec"].Meta["value_type"])

	require.NotNil(t, byName["IStore"])
	assert.Equal(t, graph.KindInterface, byName["IStore"].Kind)
	assert.Equal(t, "interface", byName["IStore"].Meta["type_flavor"])

	require.NotNil(t, byName["Color"])
	assert.Equal(t, "enum", byName["Color"].Meta["type_flavor"])

	require.NotNil(t, byName["Rec"])
	assert.Equal(t, "record", byName["Rec"].Meta["type_flavor"])

	require.NotNil(t, byName["RVec"])
	assert.Equal(t, "record", byName["RVec"].Meta["type_flavor"])
}

func TestCSharpAnonymousTypeFlavor(t *testing.T) {
	src := []byte(`namespace App;

class Host {
    void Wire() {
        var p = new { Name = "x", Age = 5 };
        System.Console.WriteLine(p.Name);
    }
}
`)
	res, err := NewCSharpExtractor().Extract("Host.cs", src)
	require.NoError(t, err)

	anon, _ := anonTypeAndExtends(t, res)
	// Dual-write: the legacy anonymous marker stays beside type_flavor.
	assert.Equal(t, true, anon.Meta["anonymous"])
	assert.Equal(t, "anonymous_class", anon.Meta["type_flavor"])
}
