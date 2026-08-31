package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestQikBasicExtractor_Basics(t *testing.T) {
	src := []byte(`' demo.qik
$INCLUDE "common.qik"

DECLARE SUB Helper (n AS INTEGER)
DECLARE FUNCTION FullName$ (first$)

TYPE Person
  first AS STRING
  age AS INTEGER
END TYPE

SUB Main
  CALL Helper(1)
  GOSUB Cleanup
  PRINT FullName$("Ada")
END SUB

FUNCTION FullName$ (first$)
  FullName$ = first$ + " Lovelace"
END FUNCTION

SUB Helper (n AS INTEGER)
  PRINT n
END SUB

Cleanup:
  PRINT "done"
  RETURN
`)
	e := NewQikBasicExtractor()
	require.Equal(t, "qikbasic", e.Language())
	require.Equal(t, []string{".qik"}, e.Extensions())

	res, err := e.Extract("demo.qik", src)
	require.NoError(t, err)

	var gotMain, gotHelper, gotFullName, gotPerson, gotCleanup, gotDeclare bool
	for _, n := range res.Nodes {
		switch n.Name {
		case "Main":
			gotMain = n.Kind == graph.KindFunction
		case "Helper":
			gotHelper = n.Kind == graph.KindFunction
		case "FullName":
			gotFullName = n.Kind == graph.KindFunction
		case "Person":
			gotPerson = n.Kind == graph.KindType
		case "Cleanup":
			gotCleanup = n.Kind == graph.KindVariable
		}
		if n.Name == "FullName" && n.Kind == graph.KindVariable {
			gotDeclare = true
		}
	}
	var gotImport, gotCallHelper, gotGosub bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::common.qik" {
			gotImport = true
		}
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::Helper" {
			gotCallHelper = true
		}
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::Cleanup" {
			gotGosub = true
		}
	}
	assert.True(t, gotMain, "Main SUB")
	assert.True(t, gotHelper, "Helper SUB")
	assert.True(t, gotFullName, "FullName FUNCTION (suffix stripped)")
	assert.True(t, gotPerson, "Person TYPE")
	assert.True(t, gotCleanup, "Cleanup label")
	assert.False(t, gotDeclare, "DECLARE must not duplicate body symbols")
	assert.True(t, gotImport, "$INCLUDE import edge")
	assert.True(t, gotCallHelper, "CALL Helper edge")
	assert.True(t, gotGosub, "GOSUB Cleanup edge")
}

func TestQikBasicExtractor_EmptyInput(t *testing.T) {
	res, err := NewQikBasicExtractor().Extract("e.qik", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	assert.Equal(t, "qikbasic", res.Nodes[0].Language)
}

func TestQikBasicExtractor_DefFn(t *testing.T) {
	src := []byte(`DEF FNDouble(x) = x * 2
PRINT FNDouble(3)
`)
	res, err := NewQikBasicExtractor().Extract("fn.qik", src)
	require.NoError(t, err)
	var got bool
	for _, n := range res.Nodes {
		if n.Name == "FNDouble" && n.Kind == graph.KindFunction {
			got = true
		}
	}
	assert.True(t, got)
}
