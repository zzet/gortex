package astquery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The MQL grammar (github.com/davalillo/tree-sitter-mql5) is a
// tree-sitter-cpp fork, so once the resolver wires "mql" every
// cpp-shaped pattern — call_expression, function_definition, … —
// must match MQL sources. Regression for: search_ast silently
// returned 0 matches for language "mql" (resolver fell through to
// nil and the plan skipped query compilation without an error).

func TestDefaultLanguageResolver_MQL(t *testing.T) {
	sl := DefaultLanguageResolver("mql")
	require.NotNil(t, sl, "mql must resolve to a tree-sitter binding")
}

// fixtureMQL is a small MQL5 file exercising the shapes the graph
// extractor and raw patterns care about: an #include, input params,
// two function definitions and a handful of call expressions.
const fixtureMQL = `//+------------------------------------------------------------------+
//| fixture.mq5                                                      |
//+------------------------------------------------------------------+
#property strict

input double RiskPercent = 1.0;

int CountPositions()
  {
   return(PositionsTotal());
  }

void CloseAll(int magic)
  {
   for(int i = PositionsTotal() - 1; i >= 0; i--)
     {
      ulong ticket = PositionGetTicket(i);
      if(ticket > 0)
         Print("closing ", ticket);
     }
  }

int OnInit()
  {
   CloseAll(12345);
   return(INIT_SUCCEEDED);
  }
`

func TestRawPattern_MQL_CallExpression(t *testing.T) {
	res, err := RunOnSource(context.Background(), Options{
		Pattern: `((call_expression function: (identifier) @fn) @match (#eq? @fn "CloseAll"))`,
	}, "ea.mq5", "mql", []byte(fixtureMQL))
	require.NoError(t, err)
	require.NotNil(t, res.Matches, "mql raw patterns must compile and match, not silently no-op")
	require.Equal(t, 1, res.Total, "expected exactly one CloseAll call site")
}

func TestRawPattern_MQL_AnyCallExpression(t *testing.T) {
	res, err := RunOnSource(context.Background(), Options{
		Pattern: `(call_expression) @match`,
	}, "ea.mq5", "mql", []byte(fixtureMQL))
	require.NoError(t, err)
	require.NotNil(t, res.Matches)
	// PositionsTotal x3, PositionGetTicket, Print, CloseAll.
	assert.GreaterOrEqual(t, res.Total, 5, "expected every call expression to match")
}

func TestRawPattern_MQL_FunctionDefinition(t *testing.T) {
	res, err := RunOnSource(context.Background(), Options{
		Pattern: `(function_definition declarator: (function_declarator declarator: (identifier) @name) @match)`,
	}, "ea.mq5", "mql", []byte(fixtureMQL))
	require.NoError(t, err)
	require.NotNil(t, res.Matches)
	assert.Equal(t, 3, res.Total, "CountPositions, CloseAll and OnInit")
}
