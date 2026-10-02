package elide

import (
	"strings"
	"testing"
)

// MQL shares the tree-sitter-mql5 grammar (a tree-sitter-cpp fork),
// so compression must elide brace bodies exactly like cpp. Regression
// for: compress_bodies returned MQL sources untouched because the
// specs map had no "mql" entry and the package is fail-soft.

func TestCompress_MQL(t *testing.T) {
	src := `//+------------------------------------------------------------------+
//| ea.mq5                                                           |
//+------------------------------------------------------------------+
#property strict

input double RiskPercent = 1.0;

// CountPositions returns the open position count.
int CountPositions()
  {
   int total = PositionsTotal();
   return(total);
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
`
	out, err := CompressString(src, "mql")
	if err != nil {
		t.Fatalf("CompressString: %v", err)
	}
	mustContain := []string{
		"#property strict",
		"input double RiskPercent = 1.0;",
		"// CountPositions returns the open position count.",
		"int CountPositions()",
		"void CloseAll(int magic)",
		"lines elided",
	}
	mustNot := []string{
		"int total = PositionsTotal();",
		"ulong ticket = PositionGetTicket(i);",
		`Print("closing ", ticket);`,
	}
	checkContains(t, out, mustContain, mustNot)
}

// Legacy MQL4 (pre-build-600) is a C subset: same brace bodies, Order* API.
func TestCompress_MQL4Legacy(t *testing.T) {
	src := `#property strict

int CountOrders()
  {
   int ot = OrdersTotal() - 1;
   return(ot);
  }

void CloseFirst()
  {
   bool ok = OrderSelect(0, SELECT_BY_POS);
   if(ok)
      OrderClose(OrderTicket(), OrderLots(), OrderClosePrice(), 3, clrRed);
  }
`
	out, err := CompressString(src, "mql")
	if err != nil {
		t.Fatalf("CompressString: %v", err)
	}
	mustContain := []string{"int CountOrders()", "void CloseFirst()", "lines elided"}
	mustNot := []string{
		"int ot = OrdersTotal() - 1;",
		"bool ok = OrderSelect(0, SELECT_BY_POS);",
	}
	checkContains(t, out, mustContain, mustNot)
}

// Signatures and doc comments stay intact while bodies shrink: the
// compressed output must be materially smaller than the input.
func TestCompress_MQL_SavesLines(t *testing.T) {
	src := strings.Repeat(`
int LoopN(int n)
  {
   int acc = 0;
   for(int i = 0; i < n; i++)
      acc += i;
   return(acc);
  }
`, 10)
	out, err := CompressString(src, "mql")
	if err != nil {
		t.Fatalf("CompressString: %v", err)
	}
	if got := strings.Count(out, "\n"); got >= strings.Count(src, "\n") {
		t.Fatalf("compression saved nothing: in=%d lines, out=%d lines",
			strings.Count(src, "\n"), got)
	}
}
