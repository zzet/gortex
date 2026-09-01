package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestCppExtractor_Function(t *testing.T) {
	src := []byte(`#include <iostream>

void greet(const std::string& name) {
    std::cout << "Hello " << name << std::endl;
}
`)
	e := NewCppExtractor()
	result, err := e.Extract("main.cpp", src)
	require.NoError(t, err)

	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	assert.GreaterOrEqual(t, len(funcs), 1)
	assert.Equal(t, "greet", funcs[0].Name)
}

func TestCppExtractor_Class(t *testing.T) {
	src := []byte(`class Point {
public:
    int x, y;

    Point(int x, int y) : x(x), y(y) {}

    int distance() {
        return x * x + y * y;
    }
};
`)
	e := NewCppExtractor()
	result, err := e.Extract("point.cpp", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	assert.GreaterOrEqual(t, len(types), 1)
	assert.Equal(t, "Point", types[0].Name)

	methods := nodesOfKind(result.Nodes, graph.KindMethod)
	assert.GreaterOrEqual(t, len(methods), 1)

	// Check MemberOf edges point to class.
	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	assert.GreaterOrEqual(t, len(memberEdges), 1)
	for _, edge := range memberEdges {
		assert.Equal(t, "point.cpp::Point", edge.To)
	}
}

func TestCppExtractor_Struct(t *testing.T) {
	src := []byte(`struct Vec3 {
    float x, y, z;
};
`)
	e := NewCppExtractor()
	result, err := e.Extract("vec.cpp", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Vec3", types[0].Name)
}

func TestCppExtractor_Include(t *testing.T) {
	src := []byte(`#include <iostream>
#include "mylib.h"
`)
	e := NewCppExtractor()
	result, err := e.Extract("main.cpp", src)
	require.NoError(t, err)

	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	assert.Len(t, imports, 2)
}

func TestCppExtractor_Namespace(t *testing.T) {
	src := []byte(`namespace math {
    int add(int a, int b) {
        return a + b;
    }
}
`)
	e := NewCppExtractor()
	result, err := e.Extract("math.cpp", src)
	require.NoError(t, err)

	pkgs := nodesOfKind(result.Nodes, graph.KindPackage)
	require.Len(t, pkgs, 1)
	assert.Equal(t, "math", pkgs[0].Name)
}

func TestCppExtractor_Enum(t *testing.T) {
	src := []byte(`enum class Color {
    Red,
    Green,
    Blue
};
`)
	e := NewCppExtractor()
	result, err := e.Extract("color.cpp", src)
	require.NoError(t, err)

	types := nodesOfKind(result.Nodes, graph.KindType)
	require.Len(t, types, 1)
	assert.Equal(t, "Color", types[0].Name)
}

func TestCppExtractor_Calls(t *testing.T) {
	src := []byte(`void greet() {}

void run() {
    greet();
}
`)
	e := NewCppExtractor()
	result, err := e.Extract("main.cpp", src)
	require.NoError(t, err)

	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	assert.GreaterOrEqual(t, len(calls), 1)
}

func TestCppExtractor_Extensions(t *testing.T) {
	e := NewCppExtractor()
	assert.Equal(t, "cpp", e.Language())
	exts := e.Extensions()
	assert.Contains(t, exts, ".cpp")
	assert.Contains(t, exts, ".cc")
	assert.Contains(t, exts, ".cxx")
	assert.Contains(t, exts, ".hpp")
	assert.Contains(t, exts, ".hxx")
	assert.NotContains(t, exts, ".h")
}

func TestCppExtractor_FnValueAddressOf(t *testing.T) {
	src := []byte("void handler() {}\n" +
		"struct Foo { void method() {} };\n" +
		"void run() {\n" +
		"  reg(&handler);\n" +
		"  bind(&Foo::method);\n" +
		"}\n")
	res, err := NewCppExtractor().Extract("s.cpp", src)
	require.NoError(t, err)

	forms := map[string]string{} // fn_value_name -> fn_ref_form
	for _, e := range res.Edges {
		if e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != "callback_candidate" {
			continue
		}
		if name, _ := e.Meta["fn_value_name"].(string); name != "" {
			form, _ := e.Meta["fn_ref_form"].(string)
			forms[name] = form
			assert.Equal(t, "s.cpp::run", e.From, "captured in the enclosing function")
		}
	}
	if _, ok := forms["handler"]; !ok {
		t.Errorf("register(&handler) should capture handler as a function value (got %v)", forms)
	}
	if _, ok := forms["Foo::method"]; !ok {
		t.Errorf("bind(&Foo::method) should capture Foo::method as a function value (got %v)", forms)
	}
	assert.Equal(t, "address_of", forms["handler"], "&handler is an address-of form")
	assert.Equal(t, "address_of", forms["Foo::method"], "&Foo::method is an address-of form")
}

func TestCppExtractor_FactoryChainReceiver(t *testing.T) {
	src := []byte("struct Widget { Widget withX() { return *this; } };\n" +
		"Widget builder() { return Widget(); }\n" +
		"void run() {\n" +
		"  builder().withX().build();\n" +
		"}\n")
	res, err := NewCppExtractor().Extract("w.cpp", src)
	require.NoError(t, err)

	var withX, build *graph.Edge
	for _, e := range res.Edges {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		switch e.To {
		case "unresolved::*.withX":
			withX = e
		case "unresolved::*.build":
			build = e
		}
	}
	require.NotNil(t, withX, "withX() call edge")
	require.NotNil(t, build, "build() call edge")
	// builder() is a typed factory, so withX()'s receiver resolves to Widget.
	assert.Equal(t, "Widget", withX.Meta["receiver_type"], "factory base resolves the chained receiver type")
	// build()'s hop (withX) is not a typed node here, so the chain receiver
	// expression is preserved for the graph-aware resolver to complete.
	if got, _ := build.Meta["receiver_expr"].(string); got != "builder().withX()" {
		t.Errorf("receiver_expr = %q, want builder().withX()", got)
	}
}

// A free template function definition lives under a template_declaration,
// so its symbol must cover the `template <…>` header and must survive
// declarator forms the plain function_definition patterns never reach
// (an explicit specialization's `name<Args>` declarator). Template
// members declared inside a class body are not free functions and stay
// on the class-body path.
func TestCppExtractor_FreeTemplateFunction(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		fn        string
		startLine int
		endLine   int
		scopeNS   string
	}{
		{
			name:      "file scope",
			src:       "template <typename Char>\nvoid vprintf(buffer<Char>& buf, basic_string_view<Char> format) {\n  use(buf);\n}\n",
			fn:        "vprintf",
			startLine: 1,
			endLine:   4,
		},
		{
			name:      "inside namespace",
			src:       "namespace detail {\ntemplate <typename Char>\nvoid vprintf(buffer<Char>& buf) {\n  use(buf);\n}\n}\n",
			fn:        "vprintf",
			startLine: 2,
			endLine:   5,
			scopeNS:   "detail",
		},
		{
			name:      "multiple template params",
			src:       "template <typename T, typename U, int N>\nauto combine(T t, U u) -> U {\n  return u;\n}\n",
			fn:        "combine",
			startLine: 1,
			endLine:   4,
		},
		{
			name:      "explicit specialization",
			src:       "template <>\nvoid render<char>(char v) {\n  use(v);\n}\n",
			fn:        "render",
			startLine: 1,
			endLine:   4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := NewCppExtractor().Extract("tpl.hpp", []byte(tc.src))
			require.NoError(t, err)

			fn := nodeNamed(t, result.Nodes, graph.KindFunction, tc.fn)
			assert.Equal(t, tc.startLine, fn.StartLine, "span covers the template header")
			assert.Equal(t, tc.endLine, fn.EndLine)
			assert.Equal(t, "tpl.hpp::"+tc.fn, fn.ID)
			if tc.scopeNS != "" {
				assert.Equal(t, tc.scopeNS, fn.Meta["scope_ns"])
			}

			var defined bool
			for _, ed := range edgesOfKind(result.Edges, graph.EdgeDefines) {
				if ed.From == "tpl.hpp" && ed.To == fn.ID {
					defined = true
				}
			}
			assert.True(t, defined, "file must define %s", fn.ID)
		})
	}
}

// A primary template and its explicit specialization are two definitions
// of the same name; both keep a node.
func TestCppExtractor_TemplateSpecializationDoesNotOverwritePrimary(t *testing.T) {
	src := []byte("template <typename T>\nvoid render(T v) {\n  use(v);\n}\n\ntemplate <>\nvoid render<char>(char v) {\n  use(v);\n}\n")
	result, err := NewCppExtractor().Extract("tpl.hpp", []byte(src))
	require.NoError(t, err)

	starts := map[int]bool{}
	for _, n := range nodesOfKind(result.Nodes, graph.KindFunction) {
		if n.Name == "render" {
			starts[n.StartLine] = true
		}
	}
	assert.Equal(t, map[int]bool{1: true, 6: true}, starts)
}

// Negative case: a template member declared inside a class body is not a
// free function — its emission is untouched by the template dispatch.
func TestCppExtractor_TemplateClassMemberUnchanged(t *testing.T) {
	src := []byte("class Holder {\n public:\n  template <typename T>\n  void put(T v) {\n    store(v);\n  }\n};\n")
	result, err := NewCppExtractor().Extract("holder.hpp", src)
	require.NoError(t, err)

	var puts []*graph.Node
	for _, n := range result.Nodes {
		if n.Name == "put" {
			puts = append(puts, n)
		}
	}
	require.Len(t, puts, 1, "no duplicate node for an in-class template member")
	assert.Equal(t, 4, puts[0].StartLine, "still spans from the member's own definition")
	assert.Equal(t, 6, puts[0].EndLine)
}

// A header-only library opens and closes its namespace through macros the
// preprocessor would expand (`FMT_BEGIN_NAMESPACE`), which tree-sitter cannot
// resolve — it reads the marker as a declaration and recovers around it. The
// free template function further down must still be extracted, carrying the
// inner `namespace detail` it sits in as its scope. This is the shape a real
// header-only formatting library ships, include guard and all.
func TestCppExtractor_TemplateFunctionUnderMacroNamespaceMarkers(t *testing.T) {
	src := []byte(`#ifndef LIB_PRINTF_H_
#define LIB_PRINTF_H_

#include "format.h"

FMT_BEGIN_NAMESPACE
FMT_BEGIN_EXPORT

template <typename Char> class basic_printf_context {
 public:
  auto out() -> basic_appender<Char> { return out_; }

 private:
  basic_appender<Char> out_;
};

namespace detail {

template <typename Char, typename Context>
void vprintf(buffer<Char>& buf, basic_string_view<Char> format,
             basic_format_args<Context> args) {
  write(buf, format);
}

}  // namespace detail

FMT_END_EXPORT
FMT_END_NAMESPACE

#endif  // LIB_PRINTF_H_
`)
	result, err := NewCppExtractor().Extract("printf.hpp", src)
	require.NoError(t, err)

	fn := nodeNamed(t, result.Nodes, graph.KindFunction, "vprintf")
	assert.Equal(t, 19, fn.StartLine, "span covers the template header")
	assert.Equal(t, 23, fn.EndLine)
	assert.Equal(t, "detail", fn.Meta["scope_ns"])
}
