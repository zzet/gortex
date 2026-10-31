package graph

import (
	"strings"
	"testing"
)

// stubKindByConcatenation is the original StubKind, which built
// k+"::" for every candidate kind on every call. It is the reference
// StubKind must keep agreeing with.
func stubKindByConcatenation(id string) string {
	kinds := []string{StubKindStdlib, StubKindExternalCall, StubKindBuiltin, StubKindModule}
	for _, k := range kinds {
		if strings.HasPrefix(id, k+"::") {
			return k
		}
	}
	first := strings.Index(id, "::")
	if first < 0 {
		return ""
	}
	rest := id[first+2:]
	for _, k := range kinds {
		if strings.HasPrefix(rest, k+"::") {
			return k
		}
	}
	return ""
}

// TestStubKind pins the stub-id grammar: a kind counts only as the
// first or second "::"-separated segment, and only when "::" follows it.
func TestStubKind(t *testing.T) {
	t.Parallel()

	cases := []struct{ id, kind string }{
		// Legacy form, no repo prefix.
		{"stdlib::fmt::Errorf", StubKindStdlib},
		{"external_call::requests.get", StubKindExternalCall},
		{"builtin::go::len", StubKindBuiltin},
		{"module::lodash", StubKindModule},
		// Per-repo form.
		{"gortex::stdlib::fmt::Errorf", StubKindStdlib},
		{"gortex::external_call::requests.get", StubKindExternalCall},
		{"tree-sitter-dart::builtin::dart::print", StubKindBuiltin},
		{"gortex::module::lodash", StubKindModule},
		// The kind segment may end the id, and the repo prefix may be empty.
		{"stdlib::", StubKindStdlib},
		{"gortex::module::", StubKindModule},
		{"::builtin::go::len", StubKindBuiltin},
		// A leading kind wins over a second-segment one.
		{"builtin::stdlib::x", StubKindBuiltin},
		// Not stubs.
		{"", ""},
		{"stdlib", ""},
		{"stdlib:", ""},
		{"stdlib:fmt", ""},
		{"gortex::stdlib", ""},
		{"gortex::stdlib:fmt", ""},
		{"Stdlib::fmt", ""},
		{"stdlibs::fmt", ""},
		{"xmodule::lodash", ""},
		{"a::b::stdlib::fmt", ""},
		{"gortex/internal/graph/stub.go::StubKind", ""},
		{"unresolved::Foo", ""},
	}
	for _, c := range cases {
		if got := StubKind(c.id); got != c.kind {
			t.Errorf("StubKind(%q) = %q, want %q", c.id, got, c.kind)
		}
		if got := IsStub(c.id); got != (c.kind != "") {
			t.Errorf("IsStub(%q) = %v, want %v", c.id, got, c.kind != "")
		}
	}
}

// TestStubKindMatchesConcatenation checks StubKind against the
// concatenating original on every prefix of ids built from each kind,
// near-miss kinds and repo prefixes, so every truncation point around
// the "::" separators is covered.
func TestStubKindMatchesConcatenation(t *testing.T) {
	t.Parallel()

	heads := []string{"", "gortex::", "::", ":", "a::b::", "stdlib::", "gortex/x.go:"}
	kinds := []string{
		StubKindStdlib, StubKindExternalCall, StubKindBuiltin, StubKindModule,
		"stdli", "stdlibs", "external", "Builtin", "unresolved",
	}
	tails := []string{"::fmt::Errorf", ":::x", "::", ":", "", "::module::x"}
	for _, head := range heads {
		for _, kind := range kinds {
			for _, tail := range tails {
				id := head + kind + tail
				for i := 0; i <= len(id); i++ {
					if got, want := StubKind(id[:i]), stubKindByConcatenation(id[:i]); got != want {
						t.Errorf("StubKind(%q) = %q, want %q", id[:i], got, want)
					}
				}
			}
		}
	}
}

// BenchmarkStubKind measures the per-candidate stub check the resolver
// runs while filtering name-lookup results, which are mostly
// first-party ids.
func BenchmarkStubKind(b *testing.B) {
	for _, bc := range []struct{ name, id string }{
		{"first_party", "gortex/internal/resolver/resolver.go::withoutAttributedTerminals"},
		{"repo_stub", "gortex::external_call::requests.get"},
		{"legacy_stub", "module::lodash"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				StubKind(bc.id)
			}
		})
	}
}
