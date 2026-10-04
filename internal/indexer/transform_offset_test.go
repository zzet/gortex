package indexer

import (
	"bytes"
	"strings"
	"testing"
)

// shrinkingPreParse is a deliberately broken offset-preserving transform: it
// drops a byte, violating the equal-length contract. The pipeline must reject
// its output to keep offsets intact.
type shrinkingPreParse struct{}

func (shrinkingPreParse) name() string        { return "shrinking" }
func (shrinkingPreParse) matches(string) bool { return true }
func (shrinkingPreParse) rewrite(_ string, src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	return src[:len(src)-1]
}

func TestTransformPipeline_OffsetPreserving(t *testing.T) {
	t.Run("builtin csharp blanker blanks directives and preserves offsets", func(t *testing.T) {
		src := []byte("class C {\n#if DEBUG\n    void Dbg() {}\n#endif\n    void M() {}\n}\n")
		p := newTransformPipeline(nil, nil, nil)
		out := p.run("a.cs", src)

		if len(out) != len(src) {
			t.Fatalf("offset preservation broken: in %d bytes, out %d bytes", len(src), len(out))
		}
		if bytes.ContainsRune(out, '#') {
			t.Errorf("directive markers were not blanked:\n%s", out)
		}
		// Guarded code on both sides of the directive survives intact.
		if !bytes.Contains(out, []byte("void Dbg() {}")) {
			t.Errorf("guarded code inside #if was lost:\n%s", out)
		}
		if !bytes.Contains(out, []byte("void M() {}")) {
			t.Errorf("code after #endif was lost:\n%s", out)
		}
		// Newlines (and therefore line numbers) are unchanged.
		if got, want := bytes.Count(out, []byte("\n")), bytes.Count(src, []byte("\n")); got != want {
			t.Errorf("newline count changed: got %d want %d", got, want)
		}
		// The byte offset of `void M` is identical before and after — the
		// whole point of an offset-preserving transform.
		if bytes.Index(out, []byte("void M")) != bytes.Index(src, []byte("void M")) {
			t.Errorf("offset of `void M` shifted: src %d out %d",
				bytes.Index(src, []byte("void M")), bytes.Index(out, []byte("void M")))
		}
	})

	t.Run("non-cs file is untouched by the csharp blanker", func(t *testing.T) {
		src := []byte("# this is a markdown heading, not a directive\n")
		p := newTransformPipeline(nil, nil, nil)
		out := p.run("a.md", src)
		if !bytes.Equal(out, src) {
			t.Errorf("markdown file was altered:\n%s", out)
		}
	})

	t.Run("length-changing pre-parse transform is rejected", func(t *testing.T) {
		src := []byte("hello world")
		p := newTransformPipeline(nil, nil, nil)
		p.prePass = append(p.prePass, shrinkingPreParse{})
		out := p.run("x.txt", src)
		if !bytes.Equal(out, src) {
			t.Errorf("a length-changing pre-parse transform must be dropped; got %q", out)
		}
	})

	t.Run("blanker is a no-op without a hash", func(t *testing.T) {
		src := []byte("class C { void M() {} }\n")
		if out := blankCSharpPreprocDirectives(src); !bytes.Equal(out, src) {
			t.Errorf("expected unchanged bytes, got %q", out)
		}
	})

	t.Run("elif and else lines are blanked too", func(t *testing.T) {
		src := []byte("#if A\nint a;\n#elif B\nint b;\n#else\nint c;\n#endif\n")
		out := blankCSharpPreprocDirectives(src)
		if len(out) != len(src) {
			t.Fatalf("length changed: %d vs %d", len(out), len(src))
		}
		for _, kw := range []string{"#if", "#elif", "#else", "#endif"} {
			if bytes.Contains(out, []byte(kw)) {
				t.Errorf("directive %q not blanked:\n%s", kw, out)
			}
		}
		for _, code := range []string{"int a;", "int b;", "int c;"} {
			if !strings.Contains(string(out), code) {
				t.Errorf("guarded code %q lost:\n%s", code, out)
			}
		}
	})
}
