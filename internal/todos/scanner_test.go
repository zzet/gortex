package todos

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func defaultTags() []string {
	return []string{"TODO", "FIXME", "HACK", "XXX", "NOTE"}
}

func TestScan_BasicMarkers(t *testing.T) {
	src := []byte(`package main
// TODO: refactor this
// FIXME(zzet): broken on Windows
# HACK: shell hack
-- NOTE: schema migration pending
/* XXX: thread-unsafe */
`)
	got := Scan(src, defaultTags(), 200)
	if len(got) != 5 {
		t.Fatalf("expected 5 findings, got %d: %+v", len(got), got)
	}
	wantTags := []string{"TODO", "FIXME", "HACK", "NOTE", "XXX"}
	for i, f := range got {
		if f.Tag != wantTags[i] {
			t.Errorf("finding %d: tag = %q, want %q", i, f.Tag, wantTags[i])
		}
	}
	if got[1].Assignee != "zzet" {
		t.Errorf("assignee = %q, want zzet", got[1].Assignee)
	}
}

func TestScan_AssigneeAndDue(t *testing.T) {
	src := []byte(`// TODO(alice)[2026-05-01]: clean up flag once cohort done #PROJ-42
`)
	got := Scan(src, defaultTags(), 200)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	f := got[0]
	if f.Assignee != "alice" {
		t.Errorf("assignee = %q", f.Assignee)
	}
	if f.Due != "2026-05-01" {
		t.Errorf("due = %q", f.Due)
	}
	if f.Ticket != "PROJ-42" {
		t.Errorf("ticket = %q", f.Ticket)
	}
	if f.Text == "" {
		t.Errorf("text should not be empty")
	}
}

func TestScan_IgnoresStringLiterals(t *testing.T) {
	src := []byte(`package main

func main() {
	s := "// TODO not a real todo"
	_ = s
}
`)
	got := Scan(src, defaultTags(), 200)
	if len(got) != 0 {
		t.Fatalf("expected no findings (string literal), got %d: %+v", len(got), got)
	}
}

func TestScan_BlockCommentContinuation(t *testing.T) {
	src := []byte(`/*
 * TODO: continued from above
 */
`)
	got := Scan(src, defaultTags(), 200)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].Tag != "TODO" || got[0].Line != 2 {
		t.Errorf("got %+v", got[0])
	}
}

func TestScan_RespectsMaxText(t *testing.T) {
	long := "// TODO: " + repeat("x", 500)
	got := Scan([]byte(long), defaultTags(), 50)
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if len(got[0].Text) != 50 {
		t.Errorf("len(text) = %d, want 50", len(got[0].Text))
	}
}

func TestScan_BytePrefilterPreservesCandidateSemantics(t *testing.T) {
	src := []byte(`value := "// TODO: marker inside a string"
	// TODO(alice)[2026-05-01]: fix parser #123
# FIXME: fix shell PROJ-42
`)
	got := Scan(src, []string{" TODO ", "FIXME"}, 200)
	want := []Finding{
		{
			Tag:      "TODO",
			Assignee: "alice",
			Due:      "2026-05-01",
			Ticket:   "#123",
			Text:     "fix parser #123",
			Line:     2,
		},
		{
			Tag:    "FIXME",
			Ticket: "PROJ-42",
			Text:   "fix shell PROJ-42",
			Line:   3,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings changed:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestScan_CandidateStringsOwnTheirStorage(t *testing.T) {
	source := []byte("// TODO(alice)[2026-05-01]: preserve this text PROJ-42\n")
	got := Scan(source, defaultTags(), 200)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	want := got[0]

	// Neither caller mutation nor a subsequent scan reusing the pooled
	// scanner buffer may alter strings retained by the first Finding.
	for i := range source {
		source[i] = 'x'
	}
	_ = Scan([]byte("// FIXME(bob): overwrite pooled scanner storage #456\n"), defaultTags(), 200)
	if got[0] != want {
		t.Fatalf("finding aliases mutable scanner storage: got %#v, want %#v", got[0], want)
	}
}

func TestScan_TaglessAllocationsDoNotScaleWithLines(t *testing.T) {
	tags := defaultTags()
	small := []byte("var ordinaryValue = 42\n")
	large := bytes.Repeat(small, 4096)

	// Warm any package-level pools before measuring.
	_ = Scan(small, tags, 200)
	_ = Scan(large, tags, 200)
	smallAllocs := testing.AllocsPerRun(20, func() {
		_ = Scan(small, tags, 200)
	})
	largeAllocs := testing.AllocsPerRun(20, func() {
		_ = Scan(large, tags, 200)
	})
	if largeAllocs > smallAllocs+1 {
		t.Fatalf("tagless allocations scale with lines: small=%.1f large=%.1f", smallAllocs, largeAllocs)
	}
}

func BenchmarkScanTaglessLarge(b *testing.B) {
	source := bytes.Repeat([]byte("var ordinaryValue = 42\n"), 64*1024)
	tags := defaultTags()
	b.ReportAllocs()
	b.SetBytes(int64(len(source)))
	b.ResetTimer()
	for range b.N {
		if got := Scan(source, tags, 200); got != nil {
			b.Fatalf("unexpected findings: %#v", got)
		}
	}
}

func TestBuildGraphArtifacts(t *testing.T) {
	findings := []Finding{
		{Tag: "TODO", Text: "do the thing", Line: 10},
		{Tag: "FIXME", Text: "fix the thing", Line: 20, Assignee: "bob", Due: "2026-06-01"},
	}
	nodes, edges := BuildGraphArtifacts("pkg/foo.go", findings, "go")
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %d", len(edges))
	}
	if nodes[0].Kind != graph.KindTodo {
		t.Errorf("node kind = %q", nodes[0].Kind)
	}
	if nodes[0].ID != "pkg/foo.go::todo:10" {
		t.Errorf("node id = %q", nodes[0].ID)
	}
	if nodes[1].Meta["assignee"] != "bob" {
		t.Errorf("meta.assignee = %v", nodes[1].Meta["assignee"])
	}
	if edges[0].Kind != graph.EdgeAnnotated {
		t.Errorf("edge kind = %q", edges[0].Kind)
	}
	if edges[0].From != "pkg/foo.go" {
		t.Errorf("edge.From = %q", edges[0].From)
	}
}

func TestBuildGraphArtifacts_DisambiguatesSameLine(t *testing.T) {
	findings := []Finding{
		{Tag: "TODO", Text: "first", Line: 10},
		{Tag: "FIXME", Text: "second", Line: 10},
	}
	nodes, _ := BuildGraphArtifacts("pkg/foo.go", findings, "go")
	if nodes[0].ID == nodes[1].ID {
		t.Errorf("expected unique IDs on same line, got %q twice", nodes[0].ID)
	}
}

func TestBuildGraphArtifacts_PreservesCallerPathSpelling(t *testing.T) {
	// The indexer keys node identity, eviction, and incremental
	// replacement by the exact relPath spelling it hands the builder
	// (OS-native separators for subdirectory files on Windows). A
	// re-spelled artifact is invisible to those sweeps and goes stale.
	//
	// The spelling under test is written out rather than composed with
	// filepath.Join: on a POSIX runner Join yields the forward-slash
	// form, which the pre-fix ToSlash call also returned, so the
	// assertion would hold with or without the fix.
	const rel = `src\data\foo.go`
	nodes, edges := BuildGraphArtifacts(rel, []Finding{{Tag: "TODO", Text: "x", Line: 7}}, "go")
	if len(nodes) != 1 || len(edges) != 1 {
		t.Fatalf("nodes = %d, edges = %d", len(nodes), len(edges))
	}
	if nodes[0].ID != rel+"::todo:7" {
		t.Errorf("node id = %q, want %q", nodes[0].ID, rel+"::todo:7")
	}
	if nodes[0].FilePath != rel {
		t.Errorf("node file path = %q, want %q", nodes[0].FilePath, rel)
	}
	if edges[0].From != rel {
		t.Errorf("edge.From = %q, want %q", edges[0].From, rel)
	}
	if edges[0].FilePath != rel {
		t.Errorf("edge file path = %q, want %q", edges[0].FilePath, rel)
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
