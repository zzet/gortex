package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// C# has TWO switch scoping rules that coexist (issue #724): an
// ordinary local declared in a section is BODY-scoped (redeclaring it
// in a sibling is CS0128 - pinned by
// TestResolveCSharpTypedLocal_AliveAcrossSwitchSections), while a
// pattern variable bound in a case label or `when` guard is
// SECTION-scoped (sibling sections may redeclare it - Roslyn accepts
// `case int x:` beside `case string x:`). Stopping every extent at
// switch_body let a label's pattern variable shadow the whole body and
// eat a same-named FIELD use in a sibling section: the field read
// vanished entirely (0 reads; the merge base wrongly minted 2 - one
// was the label's own local; the correct answer is exactly 1).
func TestResolveCSharp_LabelPatternVarScopesToItsSection(t *testing.T) {
	fieldReads := func(t *testing.T, src string) int {
		t.Helper()
		g := buildCSharpResolverGraph(t, map[string]string{"L.cs": src})
		New(g).ResolveAll()
		n := 0
		for _, e := range g.AllEdges() {
			if e == nil || e.Kind != graph.EdgeReads || e.From != "L.cs::LbFlow.Run" {
				continue
			}
			if strings.HasSuffix(e.To, ".repo") || strings.HasSuffix(e.To, "::repo") {
				n++
			}
		}
		return n
	}

	t.Run("when guard pattern", func(t *testing.T) {
		got := fieldReads(t, `namespace App {
    public class LbRepo { public int Get(int id) { return id; } }
    public class LbFlow {
        private readonly LbRepo repo;
        public int Run(object o, int k) {
            switch (k) {
                case 1 when o is LbRepo repo: return repo.Get(1);
                default: return repo.Get(2);
            }
        }
    }
}`)
		assert.Equal(t, 1, got,
			"the guard's pattern variable dies with its section - the default section's `repo` is the FIELD, exactly one field read")
	})

	t.Run("case label pattern", func(t *testing.T) {
		got := fieldReads(t, `namespace App {
    public class LbRepo { public int Get(int id) { return id; } }
    public class LbFlow {
        private readonly LbRepo repo;
        public int Run(object o, int k) {
            switch (o) {
                case LbRepo repo: return repo.Get(1);
                default: return repo.Get(2);
            }
        }
    }
}`)
		assert.Equal(t, 1, got,
			"the label's pattern variable dies with its section - the default section's `repo` is the FIELD, exactly one field read")
	})

	// A braced section body is the one shape whose first statement is a
	// `block`, not a `*_statement`: it is what the discriminator's block
	// arm exists for. Without that arm every braced section reads as
	// all-label and #724 returns in full for the idiomatic
	// `case X x: { ... }` form.
	t.Run("case label pattern, braced section bodies", func(t *testing.T) {
		got := fieldReads(t, `namespace App {
    public class LbRepo { public int Get(int id) { return id; } }
    public class LbFlow {
        private readonly LbRepo repo;
        public int Run(object o, int k) {
            switch (o) {
                case LbRepo repo: { return repo.Get(1); }
                default: { return repo.Get(2); }
            }
        }
    }
}`)
		assert.Equal(t, 1, got,
			"a braced section body is the section's first statement - the label's pattern variable still dies with its section, exactly one field read")
	})
}

// The boundary case that motivates the discriminator as POSITION IN THE
// SECTION, not ancestor node type: a pattern variable introduced by an
// `if` INSIDE a section's statement list escapes to the switch body
// (Roslyn reports CS0165 - it resolves to the local and is merely not
// definitely assigned, never falling back to a same-named field). The
// fixture would be CS0165 under Roslyn for exactly that reason; what is
// pinned here is resolution, not definite assignment: the sibling
// section's `esc.Get(2)` must bind through the body-scoped LOCAL's
// type, and the same-named DECOY field (which has no Get) must attract
// nothing.
//
// This is a companion assertion, not the discriminator's guard: an `if`
// pattern binder sits inside a statement, so a discriminator that
// section-scoped EVERY binder under switch_section would still leave it
// green. The over-correction is caught by
// TestResolveCSharpTypedLocal_AliveAcrossSwitchSections (a body-scoped
// ordinary local), which is the test to keep when touching either.
func TestResolveCSharp_SectionStatementPatternVarEscapesToBody(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{"E.cs": `namespace App {
    public class EbRepo { public int Get(int id) { return id; } }
    public class EbDecoy { public int Peek() { return 9; } }
    public class EbFlow {
        private readonly EbDecoy esc;
        public int Run(object o, int k) {
            switch (k) {
                case 1: if (o is EbRepo esc) { return esc.Get(1); } break;
                default: return esc.Get(2);
            }
            return 0;
        }
    }
}`})
	New(g).ResolveAll()

	bound := 0
	for _, to := range callsFrom(g, "E.cs::EbFlow.Run") {
		if to == "E.cs::EbRepo.Get" {
			bound++
		}
	}
	assert.Equal(t, 2, bound,
		"a statement-territory pattern variable is BODY-scoped: both Get sites bind through the local's type, the sibling section included")
}
