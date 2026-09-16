package languages

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestQikBasicExtractor_ABABasics(t *testing.T) {
	src := []byte(`;***************************************************************************************
;* Script Name  : changeExistingHotel
;* Description  : container for book hotel RQ
;***************************************************************************************

build_local_data_item securityToken
build_local_data_item bookingKey
set securityToken = 'abc'

if bookingKey = ''
  call 'qcErrorMessage'
  goto 'over'
end_if

call 'WSGetHotelDetails'
call trim
call 'copy_data_atlas'

label 'over'
return
`)
	e := NewQikBasicExtractor()
	require.Equal(t, "qikbasic", e.Language())
	require.Equal(t, []string{".qik"}, e.Extensions())

	res, err := e.Extract("Hotel/SCRIPT/changeExistingHotel.qik", src)
	require.NoError(t, err)

	var gotScript, gotLabel, gotToken, gotBooking bool
	for _, n := range res.Nodes {
		switch n.Name {
		case "changeExistingHotel":
			gotScript = n.Kind == graph.KindFunction
		case "over":
			gotLabel = n.Kind == graph.KindVariable
		case "securityToken":
			gotToken = n.Kind == graph.KindVariable
		case "bookingKey":
			gotBooking = n.Kind == graph.KindVariable
		}
	}
	var gotCallWS, gotCallTrim, gotGoto, gotImport bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::WSGetHotelDetails" {
			gotCallWS = true
		}
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::trim" {
			gotCallTrim = true
		}
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::over" {
			gotGoto = true
		}
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::WSGetHotelDetails" {
			gotImport = true
		}
	}
	assert.True(t, gotScript, "script symbol from header")
	assert.True(t, gotLabel, "label over")
	assert.True(t, gotToken, "build_local_data_item securityToken")
	assert.True(t, gotBooking, "build_local_data_item bookingKey")
	assert.True(t, gotCallWS, "call WSGetHotelDetails")
	assert.True(t, gotCallTrim, "call trim (bare)")
	assert.True(t, gotGoto, "goto over")
	assert.True(t, gotImport, "call also emits import edge")
}

func TestQikBasicExtractor_EmptyInput(t *testing.T) {
	res, err := NewQikBasicExtractor().Extract("e.qik", []byte(""))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(res.Nodes), 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
	assert.Equal(t, "qikbasic", res.Nodes[0].Language)
}

func TestQikBasicExtractor_BasenameFallback(t *testing.T) {
	src := []byte("call 'helper'\nreturn\n")
	res, err := NewQikBasicExtractor().Extract("path/to/myScript.qik", src)
	require.NoError(t, err)
	var got bool
	for _, n := range res.Nodes {
		if n.Name == "myScript" && n.Kind == graph.KindFunction {
			got = true
		}
	}
	assert.True(t, got)
}

func TestQikBasicExtractor_RegistryClaimsQik(t *testing.T) {
	reg := parser.NewRegistry()
	RegisterAll(reg)
	ext, ok := reg.GetByExtension(".qik")
	require.True(t, ok)
	assert.Equal(t, "qikbasic", ext.Language())
	ext2, ok := reg.GetByLanguage("qikbasic")
	require.True(t, ok)
	assert.Equal(t, []string{".qik"}, ext2.Extensions())
}

// Optional live check against the sibling ITS.ABA.QIK checkout when present.
func TestQikBasicExtractor_LiveABARepo(t *testing.T) {
	// From internal/parser/languages → repo root is ../../..
	root := filepath.Clean(filepath.Join("..", "..", "..", "..", "ITS.ABA.QIK"))
	sample := filepath.Join(root, "Hotel", "SCRIPT", "changeExistingHotel.qik")
	src, err := os.ReadFile(sample)
	if err != nil {
		t.Skipf("ITS.ABA.QIK sample missing: %v", err)
	}
	res, err := NewQikBasicExtractor().Extract(sample, src)
	require.NoError(t, err)
	require.Greater(t, len(res.Nodes), 5, "expect script + locals")
	var calls int
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls {
			calls++
		}
	}
	assert.Greater(t, calls, 0, "expect call edges from real script")
}
