package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
	"reflect"
	"testing"
)

func deltaNode(id string, line int, signature string) *graph.Node {
	return &graph.Node{ID: "x.go::" + id, Kind: graph.KindFunction, Name: id, FilePath: "x.go", StartLine: line, EndLine: line, Meta: map[string]any{"signature": signature}}
}
func TestFileSymbolDeltaActualBodyAndSignatureChanges(t *testing.T) {
	before := []byte("func A() { return 1 }\nfunc Removed() {}\nfunc Sig(x int) {}\n")
	after := []byte("func A() { return 2 }\nfunc Added() {}\nfunc Sig(x string) {}\n")
	old := []*graph.Node{deltaNode("A", 1, "func A()"), deltaNode("Removed", 2, "func Removed()"), deltaNode("Sig", 3, "func Sig(x int)")}
	new := []*graph.Node{deltaNode("Sig", 3, "func Sig(x string)"), deltaNode("Added", 2, "func Added()"), deltaNode("A", 1, "func A()")}
	oldBytes, _ := json.Marshal(old)
	newBytes, _ := json.Marshal(new)
	bcopy := bytes.Clone(before)
	acopy := bytes.Clone(after)
	d := buildFileSymbolDelta(before, after, old, new, true)
	want := &fileSymbolDeltaCounts{Before: 3, After: 3, Added: 1, Removed: 1, Changed: 2, TotalChanges: 4}
	if d.State != "available" || !reflect.DeepEqual(d.Counts, want) {
		t.Fatalf("missing actual source delta: %+v", d)
	}
	ids := []string{}
	for _, s := range d.Symbols {
		ids = append(ids, s.ID+":"+s.Change)
	}
	if !reflect.DeepEqual(ids, []string{"x.go::A:changed", "x.go::Added:added", "x.go::Removed:removed", "x.go::Sig:changed"}) {
		t.Fatalf("unstable preview: %v", ids)
	}
	ob, _ := json.Marshal(old)
	nb, _ := json.Marshal(new)
	if !bytes.Equal(ob, oldBytes) || !bytes.Equal(nb, newBytes) || !bytes.Equal(before, bcopy) || !bytes.Equal(after, acopy) {
		t.Fatal("mutated caller evidence")
	}
	new[0].Meta["signature"] = "mutated"
	if d.Symbols[3].Signature != "func Sig(x string)" {
		t.Fatal("preview aliases input metadata")
	}
}
func TestFileSymbolDeltaCapDoesNotCapCounts(t *testing.T) {
	var source bytes.Buffer
	var nodes []*graph.Node
	for i := 29; i >= 0; i-- {
		fmt.Fprintln(&source, "func Symbol() {}")
		nodes = append(nodes, deltaNode(fmt.Sprintf("S%02d", i), 30-i, "func Symbol()"))
	}
	d := buildFileSymbolDelta(nil, source.Bytes(), nil, nodes, true)
	if d.Counts == nil || d.Counts.Added != 30 || d.Counts.TotalChanges != 30 || len(d.Symbols) != 20 || !d.Truncated {
		t.Fatalf("preview lost full totals: %+v", d)
	}
	for i, s := range d.Symbols {
		if s.ID != fmt.Sprintf("x.go::S%02d", i) {
			t.Fatalf("not ID ordered: %+v", d.Symbols)
		}
	}
	removed := buildFileSymbolDelta(source.Bytes(), nil, nodes, nil, true)
	if removed.Counts == nil || removed.Counts.Removed != 30 || removed.Symbols[0].StartLine != 30 {
		t.Fatalf("removed preview did not retain OLD: %+v", removed)
	}
}
func TestFileSymbolDeltaUnavailableEvidence(t *testing.T) {
	source := []byte("func A() {}\n")
	for _, mode := range []string{"unknown", "nil", "duplicate", "mixed", "range", "columns", "signature", "invalid_utf8", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			old := []*graph.Node{deltaNode("A", 1, "func A()")}
			new := []*graph.Node{deltaNode("A", 1, "func A()")}
			known := true
			raw := bytes.Clone(source)
			switch mode {
			case "unknown":
				known = false
			case "nil":
				new[0] = nil
			case "duplicate":
				new = append(new, new[0])
			case "mixed":
				new[0].FilePath = "other.go"
			case "range":
				new[0].EndLine = 99
			case "columns":
				new[0].EndColumn = 999
			case "signature":
				new[0].Meta["signature"] = 12
			case "invalid_utf8":
				raw = []byte{255}
			case "ambiguous":
				new = append(new, deltaNode("B", 1, "func B()"))
			}
			d := buildFileSymbolDelta(source, raw, old, new, known)
			if d.State != "unavailable" || d.Counts != nil || d.Reason == "" || len(d.Symbols) != 0 {
				t.Fatalf("fabricated evidence: %+v", d)
			}
		})
	}
}
func TestFileSymbolDeltaLineShiftIsChanged(t *testing.T) {
	a := deltaNode("A", 1, "func A()")
	b := deltaNode("A", 2, "func A()")
	d := buildFileSymbolDelta([]byte("func A() {}\n"), []byte("\nfunc A() {}\n"), []*graph.Node{a}, []*graph.Node{b}, true)
	if d.Counts == nil || d.Counts.Changed != 1 || d.Symbols[0].StartLine != 2 {
		t.Fatalf("full node changes suppressed: %+v", d)
	}
	stable := buildFileSymbolDelta([]byte("func A() {}\n"), []byte("func A() {}\n"), []*graph.Node{a}, []*graph.Node{a}, true)
	if stable.Counts == nil || stable.Counts.TotalChanges != 0 || len(stable.Symbols) != 0 {
		t.Fatalf("unchanged input: %+v", stable)
	}
}

func TestFileSymbolDeltaPreciseColumnsAndUTF8(t *testing.T) {
	left := deltaNode("Left", 1, "")
	left.StartColumn = 0
	left.EndColumn = 3
	right := deltaNode("Right", 1, "")
	right.StartColumn = 4
	right.EndColumn = 7
	d := buildFileSymbolDelta([]byte("abc def"), []byte("abc xyz"), []*graph.Node{left, right}, []*graph.Node{left, right}, true)
	if d.Counts == nil || d.Counts.Changed != 1 || d.Symbols[0].ID != "x.go::Right" {
		t.Fatalf("precise sibling ranges conflated: %+v", d)
	}
	split := deltaNode("UTF8", 1, "")
	split.StartColumn = 1
	split.EndColumn = 2
	bad := buildFileSymbolDelta(nil, []byte("é"), nil, []*graph.Node{split}, true)
	if bad.Counts != nil || bad.Reason == "" {
		t.Fatalf("split Unicode rune accepted: %+v", bad)
	}
}
func TestFileSymbolDeltaNestedAndCrossingUnknownColumns(t *testing.T) {
	parent := deltaNode("Parent", 1, "")
	parent.EndLine = 3
	child := deltaNode("Child", 2, "")
	data := []byte("parent\nchild\nend\n")
	d := buildFileSymbolDelta(data, data, []*graph.Node{parent, child}, []*graph.Node{child, parent}, true)
	if d.Counts == nil || d.Counts.TotalChanges != 0 {
		t.Fatalf("valid containment rejected: %+v", d)
	}
	crossing := deltaNode("Crossing", 2, "")
	crossing.EndLine = 4
	data = []byte("parent\nchild\nend\ntail\n")
	d = buildFileSymbolDelta(data, data, []*graph.Node{parent, crossing}, []*graph.Node{parent, crossing}, true)
	if d.Counts != nil || d.Reason == "" {
		t.Fatalf("crossing unknown spans accepted: %+v", d)
	}
}
func TestFileSymbolDeltaUsesActualDecodedGoExtractorSpans(t *testing.T) {
	before := []byte("package demo\nfunc A() int {\n return 1\n}\nfunc B() {}\n")
	after := []byte("package demo\nfunc A() int {\n return 2\n}\nfunc B() {}\n")
	extractor := languages.NewGoExtractor()
	old, err := extractor.Extract("x.go", before)
	if err != nil {
		t.Fatal(err)
	}
	defer old.ReleaseTree()
	new, err := extractor.Extract("x.go", after)
	if err != nil {
		t.Fatal(err)
	}
	defer new.ReleaseTree()
	// Production caller owns symbol-set filtering; this fixture explicitly
	// chooses the complete function set and keeps the actual extracted nodes.
	symbols := func(nodes []*graph.Node) []*graph.Node {
		var out []*graph.Node
		for _, n := range nodes {
			if n.Kind == graph.KindFunction {
				out = append(out, n)
			}
		}
		return out
	}
	d := buildFileSymbolDelta(before, after, symbols(old.Nodes), symbols(new.Nodes), true)
	if d.Counts == nil || d.Counts.Before != 2 || d.Counts.After != 2 || d.Counts.Changed != 1 || len(d.Symbols) != 1 || d.Symbols[0].ID != "x.go::A" {
		t.Fatalf("actual extractor body edit missed: %+v", d)
	}
}
func TestFileSymbolDeltaFullNodeMetadataChanges(t *testing.T) {
	old := deltaNode("A", 1, "")
	new := deltaNode("A", 1, "")
	new.Meta["visibility"] = "exported"
	d := buildFileSymbolDelta([]byte("func A() {}"), []byte("func A() {}"), []*graph.Node{old}, []*graph.Node{new}, true)
	if d.Counts == nil || d.Counts.Changed != 1 {
		t.Fatalf("full node metadata difference suppressed: %+v", d)
	}
}

func TestFileSymbolDeltaPreciseRangesStillRejectAmbiguousOwnership(t *testing.T) {
	for _, crossing := range []bool{false, true} {
		left := deltaNode("Left", 1, "")
		left.EndColumn = 7
		right := deltaNode("Right", 1, "")
		right.EndColumn = 7
		if crossing {
			left.EndColumn = 5
			right.StartColumn = 3
			right.EndColumn = 7
		}
		d := buildFileSymbolDelta(nil, []byte("abcdefg"), nil, []*graph.Node{left, right}, true)
		if d.Counts != nil || d.State != "unavailable" {
			t.Fatalf("ambiguous precise ownership accepted crossing=%v: %+v", crossing, d)
		}
	}
}
