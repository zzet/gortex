package tstypes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// callOwnersFor parses one C# source through the binder and returns each
// call fact's authored owner keyed by the called method name (every site
// in the fixture calls a distinct name).
func callOwnersFor(t *testing.T, spec *LangSpec, src string) map[string]string {
	t.Helper()
	abs := filepath.Join(t.TempDir(), "App.cs")
	if err := os.WriteFile(abs, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, err := analyzeFile(spec, fileRef{node: &graph.Node{FilePath: "B/App.cs"}, absPath: abs})
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(facts.calls))
	for _, cf := range facts.calls {
		if _, dup := out[cf.method]; dup {
			t.Fatalf("fixture calls %s twice; keep one site per name", cf.method)
		}
		out[cf.method] = cf.owner
	}
	return out
}

// One site per member shape; the callee name doubles as the case key.
const csCallOwnerFixture = `namespace B {
    public class App {
        private Svc worker;
        private Svc seed = worker.FromFieldInit();
        public App() { worker.FromCtor(); }
        public int Quick { get { worker.FromGetter(); return 1; } set { worker.FromSetter(); } }
        public int Arrow => worker.FromExprProp();
        public int this[int i] { get { worker.FromIndexer(); return i; } }
        public event System.Action Rang { add { worker.FromEventAdd(); } remove { } }
        public void Busy() {
            worker.FromMethod();
            void Inner() { worker.FromLocalFunc(); }
            Inner();
            System.Action a = () => worker.FromLambda();
        }
    }
}
`

// The binder names, on every call fact, the member declaration that
// authored the site — spelled exactly as the extractor names that member's
// node, so the apply phase can pick the author out of a same-line tie.
// Local functions and lambdas mint no node of their own, so their calls
// stay with the enclosing member, as the extractor's stubs do.
func TestCSharpBinder_CallFactsCarryAuthoredMember(t *testing.T) {
	got := callOwnersFor(t, CSharpSpec(), csCallOwnerFixture)
	want := map[string]string{
		"FromFieldInit": "seed",
		"FromCtor":      "App.<init>",
		"FromGetter":    "Quick",
		"FromSetter":    "Quick",
		"FromExprProp":  "Arrow",
		"FromIndexer":   "this[]",
		"FromEventAdd":  "Rang",
		"FromMethod":    "Busy",
		"FromLocalFunc": "Busy",
		"FromLambda":    "Busy",
	}
	for method, owner := range want {
		if have, ok := got[method]; !ok {
			t.Errorf("%s: no call fact recorded", method)
		} else if have != owner {
			t.Errorf("%s: owner = %q, want %q", method, have, owner)
		}
	}
}

// A spec without MemberDeclName records no owner at all — the apply
// phase then breaks a tie exactly as before (line containment).
func TestBinder_NoMemberDeclNameLeavesOwnerEmpty(t *testing.T) {
	spec := CSharpSpec()
	spec.MemberDeclName = nil
	got := callOwnersFor(t, spec, csCallOwnerFixture)
	if len(got) == 0 {
		t.Fatal("fixture produced no call facts")
	}
	for method, owner := range got {
		if owner != "" {
			t.Errorf("%s: owner = %q, want empty without MemberDeclName", method, owner)
		}
	}
}
