package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/dart"
)

// TestDartAST_Debug dumps the AST to verify node types used in queries.
func TestDartAST_Debug(t *testing.T) {
	src := []byte(`import 'package:flutter/material.dart';

abstract class Animal {
  String get name;
  void speak();
}

class Dog extends Animal {
  @override
  String get name => 'Dog';

  @override
  void speak() {
    print('Woof!');
  }

  void fetch(String item) {
    print('Fetching $item');
  }
}

enum Color { red, green, blue }

mixin Swimming {
  void swim() {
    print('Swimming!');
  }
}

extension StringExt on String {
  String capitalize() {
    return '${this[0].toUpperCase()}${substring(1)}';
  }
}

void main() {
  final dog = Dog();
  dog.speak();
  dog.fetch('ball');
}

const version = '1.0.0';
`)
	lang := dart.GetLanguage()
	tree, err := parser.ParseFile(src, lang)
	require.NoError(t, err)
	defer tree.Close()

	root := tree.RootNode()
	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		indent := ""
		for i := 0; i < depth; i++ {
			indent += "  "
		}
		if n.IsNamed() {
			t.Logf("%s%s [%d:%d - %d:%d] %q", indent, n.Type(),
				n.StartPoint().Row, n.StartPoint().Column,
				n.EndPoint().Row, n.EndPoint().Column,
				truncate(n.Content(src), 60))
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), depth+1)
		}
	}
	walk(root, 0)
}

func TestDartExtractor_ClassWithMethods(t *testing.T) {
	src := []byte(`class UserService {
  Future<User> getUser(String id) async {
    return await findById(id);
  }

  void deleteUser(String id) {
    remove(id);
  }
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("user_service.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "UserService", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	names := []string{methods[0].Name, methods[1].Name}
	assert.Contains(t, names, "getUser")
	assert.Contains(t, names, "deleteUser")

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 2)
	for _, edge := range memberEdges {
		assert.Equal(t, "user_service.dart::UserService", edge.To)
	}

	// Methods must span signature + body, not the declaration line
	// alone. A one-line span (end == start) breaks source viewers
	// and the shape extractor.
	for _, m := range methods {
		if m.EndLine <= m.StartLine {
			t.Errorf("method %s has end_line (%d) <= start_line (%d) — body span missing",
				m.Name, m.EndLine, m.StartLine)
		}
	}
	// Exact expected spans given the fixture above. Changing the
	// fixture is fine; changing these numbers without is a regression.
	byName := map[string]*graph.Node{}
	for _, m := range methods {
		byName[m.Name] = m
	}
	if m := byName["getUser"]; m != nil {
		assert.Equal(t, 2, m.StartLine, "getUser start")
		assert.Equal(t, 4, m.EndLine, "getUser end (through closing brace)")
	}
	if m := byName["deleteUser"]; m != nil {
		assert.Equal(t, 6, m.StartLine, "deleteUser start")
		assert.Equal(t, 8, m.EndLine, "deleteUser end (through closing brace)")
	}
}

func TestDartExtractor_AbstractClass(t *testing.T) {
	src := []byte(`abstract class Repository {
  Future<User> findById(String id);
  Future<void> save(User user);
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("repository.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Repository", types[0].Name)
}

func TestDartExtractor_TopLevelFunction(t *testing.T) {
	src := []byte(`void greet(String name) {
  print('Hello, $name');
}

int add(int a, int b) => a + b;
`)
	e := NewDartExtractor()
	result, err := e.Extract("utils.dart", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 2)
	names := []string{funcs[0].Name, funcs[1].Name}
	assert.Contains(t, names, "greet")
	assert.Contains(t, names, "add")

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.Empty(t, methods)
}

func TestDartExtractor_Enum(t *testing.T) {
	src := []byte(`enum Status {
  active,
  inactive,
  pending;

  String get label => name.toUpperCase();
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("status.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Status", types[0].Name)
}

func TestDartExtractor_Mixin(t *testing.T) {
	src := []byte(`mixin Logging {
  void log(String message) {
    print('[LOG] $message');
  }
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("logging.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Logging", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 1)
	assert.Equal(t, "log", methods[0].Name)
}

func TestDartExtractor_Extension(t *testing.T) {
	src := []byte(`extension NumberParsing on String {
  int toInt() {
    return int.parse(this);
  }

  double toDouble() {
    return double.parse(this);
  }
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("extensions.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "NumberParsing", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methods, 2)
	names := []string{methods[0].Name, methods[1].Name}
	assert.Contains(t, names, "toInt")
	assert.Contains(t, names, "toDouble")
}

func TestDartExtractor_Imports(t *testing.T) {
	src := []byte(`import 'dart:async';
import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;
export 'src/widget.dart';

void main() {}
`)
	e := NewDartExtractor()
	result, err := e.Extract("main.dart", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 4)
}

func TestDartExtractor_CallSites(t *testing.T) {
	src := []byte(`void main() {
  print('hello');
  greet('world');
}

void greet(String name) {
  print(name);
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("main.dart", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 2, "expected at least 2 call edges")

	targets := make(map[string]bool)
	for _, c := range calls {
		targets[c.To] = true
	}
	assert.True(t, targets["unresolved::*.print"], "missing print call")
	assert.True(t, targets["unresolved::*.greet"], "missing greet call")
}

func TestDartExtractor_FlutterWidget(t *testing.T) {
	src := []byte(`import 'package:flutter/material.dart';

class MyApp extends StatelessWidget {
  const MyApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      home: Scaffold(
        body: Center(
          child: Text('Hello Flutter'),
        ),
      ),
    );
  }
}

void main() {
  runApp(const MyApp());
}
`)
	e := NewDartExtractor()
	result, err := e.Extract("main.dart", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "MyApp", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.GreaterOrEqual(t, len(methods), 1)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	require.Len(t, funcs, 1)
	assert.Equal(t, "main", funcs[0].Name)
}

func TestDartExtractor_DocAndVisibility(t *testing.T) {
	src := []byte(`/// The greeter widget.
class Greeter {
  /// Builds the widget.
  Widget build() => Container();

  void _privateHelper() {}
}

/// Top-level helper.
void hello() {}

void _internal() {}
`)
	e := NewDartExtractor()
	result, err := e.Extract("greeter.dart", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	greeter := byID["greeter.dart::Greeter"]
	require.NotNil(t, greeter)
	if greeter.Meta["visibility"] != "public" {
		t.Fatalf("Greeter.vis = %q", greeter.Meta["visibility"])
	}
	if greeter.Meta["doc"] != "The greeter widget." {
		t.Fatalf("Greeter.doc = %q", greeter.Meta["doc"])
	}

	build := byID["greeter.dart::Greeter.build"]
	require.NotNil(t, build)
	if build.Meta["doc"] != "Builds the widget." {
		t.Fatalf("build.doc = %q", build.Meta["doc"])
	}
	if build.Meta["visibility"] != "public" {
		t.Fatalf("build.vis = %q", build.Meta["visibility"])
	}

	priv := byID["greeter.dart::Greeter._privateHelper"]
	require.NotNil(t, priv)
	if priv.Meta["visibility"] != "private" {
		t.Fatalf("_privateHelper.vis = %q", priv.Meta["visibility"])
	}

	hello := byID["greeter.dart::hello"]
	require.NotNil(t, hello)
	if hello.Meta["doc"] != "Top-level helper." {
		t.Fatalf("hello.doc = %q", hello.Meta["doc"])
	}

	internalFn := byID["greeter.dart::_internal"]
	require.NotNil(t, internalFn)
	if internalFn.Meta["visibility"] != "private" {
		t.Fatalf("_internal.vis = %q", internalFn.Meta["visibility"])
	}
}

// TestUnnamedCtorNotEmittedAndInstantiationEdge is the B2 named test: an
// unnamed Dart constructor must not be emitted as a phantom Foo.Foo method, and
// `Foo()` must produce a typed EdgeInstantiates to the class. A `with` clause
// produces a mixin EdgeExtends edge.
func TestUnnamedCtorNotEmittedAndInstantiationEdge(t *testing.T) {
	src := []byte(`mixin Logger {}

class Widget with Logger {
  Widget();
  void build() {}
}

class App {
  void run() {
    var w = Widget();
  }
}
`)
	res, err := NewDartExtractor().Extract("w.dart", []byte(src))
	require.NoError(t, err)

	// No phantom unnamed-constructor method node.
	for _, n := range res.Nodes {
		if n.Kind == graph.KindMethod && n.ID == "w.dart::Widget.Widget" {
			t.Fatalf("unnamed constructor was emitted as a phantom method node %q", n.ID)
		}
	}

	// `var w = Widget()` is a construction, not a call.
	var sawInstantiate, sawMixin bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeInstantiates && e.From == "w.dart::App.run" && e.To == "w.dart::Widget" {
			sawInstantiate = true
		}
		if e.Kind == graph.EdgeExtends && e.From == "w.dart::Widget" && e.To == "unresolved::Logger" {
			if v, _ := e.Meta["via"].(string); v == "mixin" {
				sawMixin = true
			}
		}
	}
	assert.True(t, sawInstantiate, "expected an EdgeInstantiates from App.run to Widget; edges=%v", res.Edges)
	assert.True(t, sawMixin, "expected a mixin EdgeExtends from Widget to Logger; edges=%v", res.Edges)

	// The real method still resolves.
	var sawBuild bool
	for _, n := range res.Nodes {
		if n.Kind == graph.KindMethod && n.Name == "build" {
			sawBuild = true
		}
	}
	assert.True(t, sawBuild, "the real method build should still be emitted")
}

// Every dotted Dart constructor — named, factory, const-named, and redirecting
// factory — is a first-class member symbol: a KindMethod whose Name is the bare
// constructor name (not the class name), owned by the enclosing type through an
// EdgeMemberOf, and spanning at least its signature.
func TestDartExtractor_ConstructorVariants(t *testing.T) {
	src := []byte(`class TiledAtlas {
  final int n;

  TiledAtlas(this.n);

  /// Builds from a cache key.
  TiledAtlas.fromKey(String key) : n = 1;

  const TiledAtlas.empty() : n = 0;

  TiledAtlas.withBody(int x) : n = x {
    print(x);
  }

  factory TiledAtlas.fromTiledMap(int m) {
    return TiledAtlas.fromKey('x');
  }

  factory TiledAtlas.redirect() = TiledAtlas.empty;
}
`)
	res, err := NewDartExtractor().Extract("atlas.dart", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}

	cases := []struct {
		id        string
		name      string
		startLine int
		endLine   int
	}{
		{"atlas.dart::TiledAtlas.fromKey", "fromKey", 7, 7},
		{"atlas.dart::TiledAtlas.empty", "empty", 9, 9},
		{"atlas.dart::TiledAtlas.withBody", "withBody", 11, 13},
		{"atlas.dart::TiledAtlas.fromTiledMap", "fromTiledMap", 15, 17},
		{"atlas.dart::TiledAtlas.redirect", "redirect", 19, 19},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := byID[tc.id]
			require.NotNil(t, n, "constructor %s not emitted; nodes=%v", tc.id, byID)
			assert.Equal(t, graph.KindMethod, n.Kind)
			assert.Equal(t, tc.name, n.Name, "constructor name must be the bare name, not the class name")
			assert.Equal(t, "TiledAtlas", n.Meta["receiver"])
			assert.Equal(t, tc.startLine, n.StartLine, "start line")
			assert.Equal(t, tc.endLine, n.EndLine, "end line")

			var owned bool
			for _, ed := range res.Edges {
				if ed.Kind == graph.EdgeMemberOf && ed.From == tc.id && ed.To == "atlas.dart::TiledAtlas" {
					owned = true
				}
			}
			assert.True(t, owned, "constructor %s must be a member of its type", tc.id)
		})
	}

	// The doc comment above a named constructor rides the node.
	assert.Equal(t, "Builds from a cache key.", byID["atlas.dart::TiledAtlas.fromKey"].Meta["doc"])

	// The unnamed constructor stays out of the graph — `TiledAtlas(...)`
	// constructs the class and must not be hijacked by a phantom member.
	assert.Nil(t, byID["atlas.dart::TiledAtlas.TiledAtlas"],
		"the unnamed constructor must not be emitted as a member")

	// Constructor IDs are unique — a duplicate would collide in the store.
	ids := map[string]bool{}
	for _, n := range res.Nodes {
		require.False(t, ids[n.ID], "duplicate node id %s", n.ID)
		ids[n.ID] = true
	}
}

// A const constructor on an enum and a private named constructor are ordinary
// members too; a plain field declaration next to them is a field, never a
// constructor.
func TestDartExtractor_ConstructorsInEnumAndPrivate(t *testing.T) {
	src := []byte(`enum Status {
  active,
  inactive;

  const Status.raw();
}

class Cache {
  final int size;
  Cache._internal(this.size);
}
`)
	res, err := NewDartExtractor().Extract("status.dart", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}

	raw := byID["status.dart::Status.raw"]
	require.NotNil(t, raw, "enum const constructor must be emitted")
	assert.Equal(t, "raw", raw.Name)

	internal := byID["status.dart::Cache._internal"]
	require.NotNil(t, internal, "private named constructor must be emitted")
	assert.Equal(t, "private", internal.Meta["visibility"])

	// `final int size;` is a field declaration, not a constructor.
	size := byID["status.dart::Cache.size"]
	require.NotNil(t, size, "field declaration must be emitted")
	assert.Equal(t, graph.KindField, size.Kind)
	assert.Nil(t, byID["status.dart::Cache.Cache"])
}

// A Capitalized call-chain head names a type, so the call edge carries it as
// receiver_type. Lowercase heads, bare calls, deeper chains, and import-alias
// heads stay unstamped.
func TestDartExtractor_CapitalizedChainReceiverType(t *testing.T) {
	src := []byte(`import 'package:http/http.dart' as http;

class TiledAtlas {
  factory TiledAtlas.fromTiledMap(int m) => TiledAtlas.fromKey('x');
  TiledAtlas.fromKey(String k);
}

void main() {
  TiledAtlas.fromTiledMap(1);
  http.get('u');
  helper.run();
  bare();
  Config.opts.apply();
}
`)
	res, err := NewDartExtractor().Extract("main.dart", src)
	require.NoError(t, err)

	recvType := map[string]any{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && ed.From == "main.dart::main" {
			recvType[ed.To] = ed.Meta["receiver_type"]
		}
	}

	assert.Equal(t, "TiledAtlas", recvType["unresolved::*.fromTiledMap"],
		"a Capitalized chain head must be stamped as the receiver type")
	assert.Nil(t, recvType["unresolved::*.run"], "a lowercase head is a local, not a type")
	assert.Nil(t, recvType["unresolved::*.bare"], "a bare call has no receiver")
	assert.Nil(t, recvType["unresolved::*.apply"], "only a two-segment chain types the callee")
	assert.Nil(t, recvType["unresolved::extern::package:http/http.dart::get"],
		"an import alias is a library prefix, not a type")
}

func TestDartExtractor_FactoryChainReceiver(t *testing.T) {
	src := []byte("Widget builder() { return Widget(); }\n" +
		"void run() {\n" +
		"  builder().withX().build();\n" +
		"}\n")
	res, err := NewDartExtractor().Extract("w.dart", src)
	if err != nil {
		t.Fatal(err)
	}
	var build *graph.Edge
	seen := map[string]int{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls {
			seen[e.To]++
			if e.To == "unresolved::*.build" {
				build = e
			}
		}
	}
	if build == nil {
		t.Fatal("build() call edge not found")
	}
	if got, _ := build.Meta["receiver_expr"].(string); got != "builder().withX()" {
		t.Errorf("receiver_expr = %q, want builder().withX()", got)
	}
	if seen["unresolved::*.build"] != 1 {
		t.Errorf("build() emitted %d times, want exactly 1 (no double-count)", seen["unresolved::*.build"])
	}
}
