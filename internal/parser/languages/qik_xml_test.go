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

const sampleQikDataItem = `<?xml version="1.0" encoding="UTF-8"?>
<LocalDescRef version="1.1">
  <name>system_current_date</name>
  <type>1</type>
  <AppObjectDesc class="DATAITEM">
    <name>system_current_date</name>
    <maxLength>10</maxLength>
    <created>2005-06-16 22:29:23.0 GMT</created>
    <lastSaved>2005-06-16 22:29:23.0 GMT</lastSaved>
  </AppObjectDesc>
</LocalDescRef>
`

const sampleQikTable = `<?xml version="1.0" encoding="UTF-8"?>
<LocalDescRef version="1.1">
  <name>SEGMENT_TABLE</name>
  <type>12</type>
  <AppObjectDesc class="TABLE">
    <name>SEGMENT_TABLE</name>
    <created>2005-01-01 00:00:00.0 GMT</created>
    <lastSaved>2005-01-01 00:00:00.0 GMT</lastSaved>
    <columns>
      <ColumnDesc>
        <name>segment</name>
        <maxLength>32</maxLength>
        <cells/>
      </ColumnDesc>
      <ColumnDesc>
        <name>carrier</name>
        <maxLength>3</maxLength>
        <cells/>
      </ColumnDesc>
    </columns>
  </AppObjectDesc>
</LocalDescRef>
`

func TestIsQikXML(t *testing.T) {
	require.True(t, IsQikXML([]byte(sampleQikDataItem)))
	require.True(t, IsQikXML([]byte(sampleQikTable)))
	require.False(t, IsQikXML([]byte(`<config><x/></config>`)))
	require.False(t, IsQikXML([]byte(`<mapper namespace="com.app.X"></mapper>`)))
}

func TestQikXMLExtractor_DataItem(t *testing.T) {
	res, err := NewQikXMLExtractor().Extract("DATAITEM/system_current_date.xml", []byte(sampleQikDataItem))
	require.NoError(t, err)
	var file, item *graph.Node
	for _, n := range res.Nodes {
		switch n.Kind {
		case graph.KindFile:
			file = n
		case graph.KindVariable:
			if n.Name == "system_current_date" {
				item = n
			}
		}
	}
	require.NotNil(t, file)
	require.NotNil(t, item)
	assert.Equal(t, "DATAITEM", file.Meta["qik_class"])
	assert.Equal(t, "system_current_date", file.Meta["qik_object"])
	assert.Equal(t, "DATAITEM", item.Meta["qik_class"])
	assert.Equal(t, "10", item.Meta["qik_max_length"])
}

func TestQikXMLExtractor_TableColumns(t *testing.T) {
	res, err := NewQikXMLExtractor().Extract("TABLE/SEGMENT_TABLE.xml", []byte(sampleQikTable))
	require.NoError(t, err)
	var table *graph.Node
	cols := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		switch {
		case n.Kind == graph.KindType && n.Name == "SEGMENT_TABLE":
			table = n
		case n.Kind == graph.KindVariable && n.Meta != nil && n.Meta["qik_role"] == "column":
			cols[n.Name] = n
		}
	}
	require.NotNil(t, table)
	assert.Equal(t, "TABLE", table.Meta["qik_class"])
	require.Contains(t, cols, "segment")
	require.Contains(t, cols, "carrier")
	assert.Equal(t, "32", cols["segment"].Meta["qik_max_length"])
}

func TestQikXMLExtractor_NonQikYieldsFileOnly(t *testing.T) {
	res, err := NewQikXMLExtractor().Extract("plain.xml", []byte(`<?xml version="1.0"?><config/>`))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}

func TestQikXMLExtractor_RegistryAndSniff(t *testing.T) {
	reg := parser.NewRegistry()
	RegisterAll(reg)
	// Content-based language detection is in package parser; registry still
	// lists qikxml by language id.
	ext, ok := reg.GetByLanguage("qikxml")
	require.True(t, ok)
	assert.Empty(t, ext.Extensions())
}

func TestQikXMLExtractor_LiveABARepo(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "..", "..", "ITS.ABA.QIK"))
	di := filepath.Join(root, "Hotel", "DATAITEM", "system_current_date.xml")
	src, err := os.ReadFile(di)
	if err != nil {
		t.Skipf("ITS.ABA.QIK sample missing: %v", err)
	}
	require.True(t, IsQikXML(src))
	res, err := NewQikXMLExtractor().Extract(di, src)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(res.Nodes), 2)

	tbl := filepath.Join(root, "Hotel", "TABLE", "SEGMENT_TABLE.xml")
	tsrc, err := os.ReadFile(tbl)
	if err != nil {
		t.Skip(err)
	}
	tres, err := NewQikXMLExtractor().Extract(tbl, tsrc)
	require.NoError(t, err)
	var cols int
	for _, n := range tres.Nodes {
		if n.Meta != nil && n.Meta["qik_role"] == "column" {
			cols++
		}
	}
	assert.Greater(t, cols, 0, "expect table columns")
}
