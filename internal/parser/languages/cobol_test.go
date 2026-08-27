package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestCobolExtractor_Program(t *testing.T) {
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. HELLO-WORLD.
       DATA DIVISION.
       WORKING-STORAGE SECTION.
       COPY COMMONLIB.
       PROCEDURE DIVISION.
       MAIN SECTION.
           CALL 'GREET' USING NAME.
           STOP RUN.
`)
	e := NewCobolExtractor()
	require.Equal(t, "cobol", e.Language())

	res, err := e.Extract("HELLO.cob", src)
	require.NoError(t, err)

	var gotProg, gotDiv, gotSection bool
	for _, n := range res.Nodes {
		switch n.Name {
		case "HELLO-WORLD":
			gotProg = true
		case "PROCEDURE-DIVISION":
			gotDiv = true
		case "MAIN-SECTION", "WORKING-STORAGE-SECTION":
			gotSection = true
		}
	}
	var gotCall, gotCopy bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && ed.To == "unresolved::GREET" {
			gotCall = true
		}
		if ed.Kind == graph.EdgeImports && ed.To == "unresolved::import::COMMONLIB" {
			gotCopy = true
		}
	}
	assert.True(t, gotProg)
	assert.True(t, gotDiv)
	assert.True(t, gotSection)
	assert.True(t, gotCall)
	assert.True(t, gotCopy)
}

func TestCobolExtractor_Paragraphs(t *testing.T) {
	// Paragraph labels sit in area A (column 8, i.e. 7 leading spaces in
	// fixed format); statements sit in area B (column 12+). MAIN-PARA
	// PERFORMs SECOND-PARA, which is defined later in the file.
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. PARADEMO.
       PROCEDURE DIVISION.
       MAIN-PARA.
           DISPLAY 'START'.
           PERFORM SECOND-PARA.
           GO TO EXIT-PARA.
       SECOND-PARA.
           DISPLAY 'SECOND'.
       EXIT-PARA.
           STOP RUN.
`)
	e := NewCobolExtractor()
	res, err := e.Extract("PARA.cob", src)
	require.NoError(t, err)

	paras := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindFunction && n.Meta != nil && n.Meta["cobol_kind"] == "paragraph" {
			paras[n.Name] = true
		}
	}
	assert.True(t, paras["MAIN-PARA"], "MAIN-PARA paragraph node")
	assert.True(t, paras["SECOND-PARA"], "SECOND-PARA paragraph node")
	assert.True(t, paras["EXIT-PARA"], "EXIT-PARA paragraph node")
	// PROGRAM-ID is also KindFunction but must NOT be tagged a paragraph.
	for _, n := range res.Nodes {
		if n.Name == "PARADEMO" {
			assert.True(t, n.Meta == nil || n.Meta["cobol_kind"] != "paragraph",
				"PROGRAM-ID must not be a paragraph")
		}
	}

	var performEdge, goToEdge bool
	for _, ed := range res.Edges {
		if ed.Kind != graph.EdgeCalls {
			continue
		}
		if ed.From == "PARA.cob::MAIN-PARA" && ed.To == "PARA.cob::SECOND-PARA" {
			performEdge = true
		}
		if ed.From == "PARA.cob::MAIN-PARA" && ed.To == "PARA.cob::EXIT-PARA" {
			goToEdge = true
		}
	}
	assert.True(t, performEdge, "EdgeCalls MAIN-PARA -> SECOND-PARA (PERFORM)")
	assert.True(t, goToEdge, "EdgeCalls MAIN-PARA -> EXIT-PARA (GO TO)")
}

func TestCobolExtractor_ProcedureDivisionUsing(t *testing.T) {
	// A PROCEDURE DIVISION that takes parameters puts the period after the
	// parameter list, not after DIVISION. Every fixture above uses the bare
	// `PROCEDURE DIVISION.`, which is why a regex anchored on `DIVISION\.`
	// passed its tests while skipping the procedure division of real
	// programs -- and with it every paragraph and PERFORM edge.
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. USINGDEMO.
       DATA DIVISION.
       LINKAGE SECTION.
       01  LK-PARM              PIC X(8).
       PROCEDURE DIVISION USING LK-PARM.
       0001-MAIN.
           PERFORM 0002-WORK.
       0002-WORK.
           STOP RUN.
`)
	e := NewCobolExtractor()
	res, err := e.Extract("USING.cob", src)
	require.NoError(t, err)

	var gotProcDiv bool
	paras := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Name == "PROCEDURE-DIVISION" {
			gotProcDiv = true
		}
		if n.Kind == graph.KindFunction && n.Meta != nil && n.Meta["cobol_kind"] == "paragraph" {
			paras[n.Name] = true
		}
	}
	assert.True(t, gotProcDiv, "PROCEDURE-DIVISION node for a USING header")
	assert.True(t, paras["0001-MAIN"], "paragraph after a USING header")
	assert.True(t, paras["0002-WORK"], "second paragraph after a USING header")

	var performEdge bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls &&
			ed.From == "USING.cob::0001-MAIN" && ed.To == "USING.cob::0002-WORK" {
			performEdge = true
		}
	}
	assert.True(t, performEdge, "EdgeCalls 0001-MAIN -> 0002-WORK (PERFORM)")
}

func TestCobolExtractor_DivisionWordBoundary(t *testing.T) {
	// `DIVISION\b` must not turn a data name containing DIVISION into a
	// division header. WS-DIVISION-ID is a field, and DIVISIONS is not
	// DIVISION.
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. BOUNDARY.
       DATA DIVISION.
       WORKING-STORAGE SECTION.
       01  WS-DIVISION-ID       PIC X(3).
       01  WS-DIVISIONS-COUNT   PIC 9(2).
       PROCEDURE DIVISION.
       0001-MAIN.
           MOVE 'A' TO WS-DIVISION-ID.
           STOP RUN.
`)
	e := NewCobolExtractor()
	res, err := e.Extract("BOUND.cob", src)
	require.NoError(t, err)

	divs := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Kind == graph.KindType {
			divs[n.Name] = true
		}
	}
	assert.True(t, divs["PROCEDURE-DIVISION"])
	assert.True(t, divs["DATA-DIVISION"])
	assert.False(t, divs["WS-DIVISIONS-COUNT-DIVISION"], "DIVISIONS is not DIVISION")
	for name := range divs {
		assert.NotContains(t, name, "WS-", "no data name became a division header")
	}
}

func TestCobolExtractor_CopyIDMS(t *testing.T) {
	// The IDMS DML precompiler's COPY takes the copybook name as a trailing
	// operand, optionally behind RECORD. Capturing the first word made every
	// one of these an import of "IDMS".
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. IDMSDEMO.
       DATA DIVISION.
       WORKING-STORAGE SECTION.
       COPY IDMS SUBSCHEMA-CTRL.
       COPY IDMS RECORD CUST-REC-CREDIT.
       COPY PLAINBOOK.
       PROCEDURE DIVISION.
       0001-MAIN.
           STOP RUN.
`)
	e := NewCobolExtractor()
	res, err := e.Extract("IDMS.cob", src)
	require.NoError(t, err)

	imports := map[string]bool{}
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeImports {
			imports[ed.To] = true
		}
	}
	assert.True(t, imports["unresolved::import::SUBSCHEMA-CTRL"], "COPY IDMS <name>")
	assert.True(t, imports["unresolved::import::CUST-REC-CREDIT"], "COPY IDMS RECORD <name>")
	assert.True(t, imports["unresolved::import::PLAINBOOK"], "plain COPY still works")
	assert.False(t, imports["unresolved::import::IDMS"], "IDMS is not the copybook")
	assert.False(t, imports["unresolved::import::RECORD"], "RECORD is not the copybook")
}

func TestCobolExtractor_DynamicCall(t *testing.T) {
	// CALL through a data name is the majority form in real estates. The
	// edge names the identifier, not a program: resolving it needs the
	// VALUE clause.
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. CALLDEMO.
       PROCEDURE DIVISION.
       0001-MAIN.
           CALL 'MQCONN' USING HCONN.
           CALL DCC888-LIT USING WS-PARM.
           STOP RUN.
`)
	e := NewCobolExtractor()
	res, err := e.Extract("CALL.cob", src)
	require.NoError(t, err)

	var literal, dynamic *graph.Edge
	for _, ed := range res.Edges {
		switch ed.To {
		case "unresolved::MQCONN":
			literal = ed
		case "unresolved::dyncall::DCC888-LIT":
			dynamic = ed
		}
	}
	require.NotNil(t, literal, "quoted CALL stays in the literal namespace")
	require.NotNil(t, dynamic, "CALL <identifier> is captured")
	assert.Equal(t, graph.EdgeCalls, dynamic.Kind)
	assert.Equal(t, graph.OriginASTInferred, dynamic.Origin,
		"a dynamic target is inferred, not resolved")
	// The two regexes must partition, never double-count.
	assert.Nil(t, findEdgeTo(res.Edges, "unresolved::dyncall::MQCONN"))
}

func TestCobolExtractor_IgnoresCommentedOutCode(t *testing.T) {
	// COPY and CALL are unanchored and scan the whole source, so a
	// commented-out COPY or a sentence containing CALL became a real edge.
	// On one corpus that was 59% of all COPY matches.
	src := []byte(`       IDENTIFICATION DIVISION.
       PROGRAM-ID. CMTDEMO.
       DATA DIVISION.
      *    COPY DEADBOOK.
      /    COPY SLASHBOOK.
      *    CALL 'GHOST' USING X.
      *    WE CALL THE ROUTINE WHEN THE OPEN FAILED
       WORKING-STORAGE SECTION.
       COPY LIVEBOOK.
       PROCEDURE DIVISION.
       0001-MAIN.
           STOP RUN.
`)
	e := NewCobolExtractor()
	res, err := e.Extract("CMT.cob", src)
	require.NoError(t, err)

	assert.NotNil(t, findEdgeTo(res.Edges, "unresolved::import::LIVEBOOK"),
		"live COPY is kept")
	for _, dead := range []string{
		"unresolved::import::DEADBOOK",
		"unresolved::import::SLASHBOOK",
		"unresolved::GHOST",
		"unresolved::dyncall::THE",
	} {
		assert.Nil(t, findEdgeTo(res.Edges, dead), "commented out: %s", dead)
	}
}

// findEdgeTo returns the first edge pointing at to, or nil.
func findEdgeTo(edges []*graph.Edge, to string) *graph.Edge {
	for _, ed := range edges {
		if ed.To == to {
			return ed
		}
	}
	return nil
}

func TestCobolExtractor_PerformThru(t *testing.T) {
	src := []byte(`       PROGRAM-ID. THRUDEMO.
       PROCEDURE DIVISION.
       DRIVER.
           PERFORM STEP-1 THRU STEP-2.
       STEP-1.
           DISPLAY 'A'.
       STEP-2.
           DISPLAY 'B'.
`)
	res, err := NewCobolExtractor().Extract("THRU.cob", src)
	require.NoError(t, err)
	var thru1, thru2 bool
	for _, ed := range res.Edges {
		if ed.Kind == graph.EdgeCalls && ed.From == "THRU.cob::DRIVER" {
			if ed.To == "THRU.cob::STEP-1" {
				thru1 = true
			}
			if ed.To == "THRU.cob::STEP-2" {
				thru2 = true
			}
		}
	}
	assert.True(t, thru1, "PERFORM ... THRU emits edge to first paragraph")
	assert.True(t, thru2, "PERFORM ... THRU emits edge to range-end paragraph")
}

func TestCobolExtractor_EmptyInput(t *testing.T) {
	res, err := NewCobolExtractor().Extract("e.cbl", []byte(""))
	require.NoError(t, err)
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, graph.KindFile, res.Nodes[0].Kind)
}
