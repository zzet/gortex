package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// R6 (PR #432 review): owner attribution asked findEnclosingFunc's
// linear scan once per local / call / type use — an O(locals×functions)
// product on member-heavy files. The sorted lookup must answer the same
// stabbing queries, innermost-first for overlapping ranges.
func TestCSharpFuncLookup(t *testing.T) {
	l := newCSharpFuncLookup([]funcRange{
		{id: "f.cs::A.M1", startLine: 10, endLine: 20},
		{id: "f.cs::A.M2", startLine: 25, endLine: 30},
		// A local function nested inside M3 — the innermost owner wins.
		{id: "f.cs::A.M3", startLine: 40, endLine: 60},
		{id: "f.cs::A.M3.Local", startLine: 45, endLine: 50},
		{id: "f.cs::A.M4", startLine: 65, endLine: 65},
	}, nil)

	assert.Equal(t, "f.cs::A.M1", l.enclosing(10), "start boundary is inside")
	assert.Equal(t, "f.cs::A.M1", l.enclosing(20), "end boundary is inside")
	assert.Equal(t, "", l.enclosing(22), "a gap between methods has no owner")
	assert.Equal(t, "f.cs::A.M2", l.enclosing(27))
	assert.Equal(t, "f.cs::A.M3", l.enclosing(42), "before the local function the method owns the line")
	assert.Equal(t, "f.cs::A.M3.Local", l.enclosing(47), "inside the local function the innermost range wins")
	assert.Equal(t, "f.cs::A.M3", l.enclosing(55), "after the local function ownership returns to the method")
	assert.Equal(t, "f.cs::A.M4", l.enclosing(65), "a single-line method matches its own line")
	assert.Equal(t, "", l.enclosing(5), "before every range")
	assert.Equal(t, "", l.enclosing(100), "after every range")
	assert.Equal(t, "", newCSharpFuncLookup(nil, nil).enclosing(1), "empty set")
}

// Two expression-bodied members on one line produce identical ranges —
// the tie must go to the first in extraction order, deterministically
// (findEnclosingFunc's behavior), not to whatever the sort left last.
func TestCSharpFuncLookup_EqualRangeTieIsFirstInOrder(t *testing.T) {
	l := newCSharpFuncLookup([]funcRange{
		{id: "f.cs::A.First", startLine: 12, endLine: 12},
		{id: "f.cs::A.Second", startLine: 12, endLine: 12},
	}, nil)
	assert.Equal(t, "f.cs::A.First", l.enclosing(12))
}
