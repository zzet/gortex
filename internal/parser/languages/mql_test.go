package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// TestMQLExtractor_HappyPath covers the core MQL5 extraction surface: a
// class with a base class and an inline method, an interface with declared
// methods, the input/sinput/extern configuration variables, an enum, an
// include, and a free function issuing both a free call and a member call.
func TestMQLExtractor_HappyPath(t *testing.T) {
	src := []byte(`#include "Trade\Trade.mqh"

enum ENUM_STATE
  {
   STATE_IDLE,
   STATE_RUNNING
  };

class CMyExpert : public CObject
  {
public:
   int GetState() const { return m_state; }

private:
   int m_state;
  };

interface IExecutor
  {
   void Execute();
   bool Validate() const;
  };

input int    InpLots   = 1;
sinput double InpTake  = 2.0;
extern string InpComment = "";

void UpdateState()
  {
  }

void OnTick()
  {
   UpdateState();
   m_expert.Run();
  }
`)
	e := NewMQLExtractor()
	result, err := e.Extract("expert.mq5", src)
	require.NoError(t, err)

	// File node is stamped with the mql5 dialect (by extension).
	files := nodesOfKind(result.Nodes, graph.KindFile)
	require.Len(t, files, 1)
	assert.Equal(t, "expert.mq5", files[0].ID)
	assert.Equal(t, "mql5", files[0].Meta["dialect"])

	// Class: KindType with class flavor and base class on scope_parent.
	classes := nodesOfKind(result.Nodes, graph.KindType)
	var classNode, enumNode *graph.Node
	for _, n := range classes {
		switch n.Name {
		case "CMyExpert":
			classNode = n
		case "ENUM_STATE":
			enumNode = n
		}
	}
	require.NotNil(t, classNode, "class CMyExpert node missing")
	assert.Equal(t, "class", classNode.Meta["type_flavor"])
	assert.Equal(t, "CObject", classNode.Meta["scope_parent"])

	// Enum: KindType with enum flavor.
	require.NotNil(t, enumNode, "enum ENUM_STATE node missing")
	assert.Equal(t, "enum", enumNode.Meta["type_flavor"])

	// Interface: KindInterface with the two declared methods on Meta.
	ifaces := nodesOfKind(result.Nodes, graph.KindInterface)
	require.Len(t, ifaces, 1)
	assert.Equal(t, "IExecutor", ifaces[0].Name)
	methods, ok := ifaces[0].Meta["methods"].([]string)
	require.True(t, ok, "Meta[\"methods\"] should be []string, got %T", ifaces[0].Meta["methods"])
	assert.ElementsMatch(t, []string{"Execute", "Validate"}, methods)

	// Inline method: KindMethod with receiver/scope_class and a member_of
	// edge to the owning class.
	methodsNodes := nodesOfKind(result.Nodes, graph.KindMethod)
	require.Len(t, methodsNodes, 1)
	m := methodsNodes[0]
	assert.Equal(t, "GetState", m.Name)
	assert.Equal(t, "expert.mq5::CMyExpert.GetState", m.ID)
	assert.Equal(t, "CMyExpert", m.Meta["receiver"])
	assert.Equal(t, "CMyExpert", m.Meta["scope_class"])

	memberEdges := edgesOfKind(result.Edges, graph.EdgeMemberOf)
	require.Len(t, memberEdges, 1)
	assert.Equal(t, "expert.mq5::CMyExpert.GetState", memberEdges[0].From)
	assert.Equal(t, "expert.mq5::CMyExpert", memberEdges[0].To)

	// input/sinput/extern top-level variables: KindVariable with the
	// storage class on Meta.
	vars := nodesOfKind(result.Nodes, graph.KindVariable)
	require.Len(t, vars, 3)
	storage := map[string]string{}
	for _, v := range vars {
		storage[v.Name] = v.Meta["storage_class"].(string)
	}
	assert.Equal(t, "input", storage["InpLots"])
	assert.Equal(t, "sinput", storage["InpTake"])
	assert.Equal(t, "extern", storage["InpComment"])

	// Free functions: UpdateState and OnTick.
	funcs := nodesOfKind(result.Nodes, graph.KindFunction)
	funcNames := map[string]bool{}
	for _, f := range funcs {
		funcNames[f.Name] = true
	}
	assert.True(t, funcNames["UpdateState"], "free function UpdateState missing")
	assert.True(t, funcNames["OnTick"], "free function OnTick missing")

	// Include: imports edge to the unresolved target with include_kind.
	imports := edgesOfKind(result.Edges, graph.EdgeImports)
	require.Len(t, imports, 1)
	assert.Equal(t, "expert.mq5", imports[0].From)
	assert.Equal(t, "unresolved::import::Trade\\Trade.mqh", imports[0].To)
	assert.Equal(t, "quoted", imports[0].Meta["include_kind"])

	// Calls: OnTick calls the free function UpdateState and the member
	// method Run — both attributed to OnTick.
	calls := edgesOfKind(result.Edges, graph.EdgeCalls)
	require.Len(t, calls, 2)
	callTargets := map[string]bool{}
	for _, c := range calls {
		assert.Equal(t, "expert.mq5::OnTick", c.From, "call edge must be attributed to OnTick")
		callTargets[c.To] = true
	}
	assert.True(t, callTargets["unresolved::UpdateState"], "free call edge missing, got %v", callTargets)
	assert.True(t, callTargets["unresolved::*."+"Run"], "member call edge missing, got %v", callTargets)
}

// TestMQLExtractor_EmptyInput: an empty file yields the file node only.
func TestMQLExtractor_EmptyInput(t *testing.T) {
	e := NewMQLExtractor()
	result, err := e.Extract("empty.mq4", []byte(""))
	require.NoError(t, err)

	require.Len(t, result.Nodes, 1)
	assert.Equal(t, graph.KindFile, result.Nodes[0].Kind)
	assert.Empty(t, result.Edges)
}

// TestMQLExtractor_InputDeclaratorForms: every declarator of an input/
// sinput/extern declaration must mint a variable — array declarators
// (`input double Rates[][6];`), comma-separated declarators
// (`extern int A, B;`) and pointer declarators (`extern CArrayObj *Ptr;`)
// included. Before the fix these were silently dropped: only initialized
// declarators (`input int X = 5;`) survived.
func TestMQLExtractor_InputDeclaratorForms(t *testing.T) {
	src := []byte(`input double Rates[][6];
extern int A, B;
extern CArrayObj *Ptr;
input int X = 5;
`)
	e := NewMQLExtractor()
	result, err := e.Extract("inputs.mq5", src)
	require.NoError(t, err)

	want := map[string]string{
		"Rates": "input",
		"A":     "extern",
		"B":     "extern",
		"Ptr":   "extern",
		"X":     "input",
	}
	got := map[string]string{}
	for _, n := range nodesOfKind(result.Nodes, graph.KindVariable) {
		sc, ok := n.Meta["storage_class"].(string)
		require.True(t, ok, "variable %s missing storage_class meta", n.Name)
		got[n.Name] = sc
	}
	assert.Equal(t, want, got)
}

// TestMQLDialect_Stamping: dialect comes from the extension for .mq4/.mq5,
// and from content sniffing for .mqh headers (MQL5 markers win, otherwise
// the documented mql4 default).
func TestMQLDialect_Stamping(t *testing.T) {
	e := NewMQLExtractor()
	src := []byte("void f() {}\n")

	cases := []struct {
		name     string
		filePath string
		src      []byte
		want     string
	}{
		{"mq4 by extension", "legacy.mq4", src, "mql4"},
		{"mq5 by extension", "modern.mq5", src, "mql5"},
		{"mqh with mql5 marker", "header.mqh", []byte("union U { int a; };\n"), "mql5"},
		{"mqh without markers defaults to mql4", "plain.mqh", src, "mql4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := e.Extract(tc.filePath, tc.src)
			require.NoError(t, err)
			files := nodesOfKind(result.Nodes, graph.KindFile)
			require.Len(t, files, 1)
			assert.Equal(t, tc.want, files[0].Meta["dialect"])
		})
	}
}
