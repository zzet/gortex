package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The POSITIVE direction of csharpCollectExtraBindingScopes: every
// binding form the file indexes must SHADOW a same-named field inside
// its extent - the miss mints a spurious field read on an
// injected-repository-style name. The committed tests all asserted the
// negative direction (a use outside the extent keeps its edge), so
// no-op'ing the whole collector left both packages green (issue #726).
//
// One case per switch arm. Cases whose binder scopes to its own
// statement (foreach, for, using, catch, lambda, local-function
// parameters, query clauses) also place a call AFTER the extent and
// expect exactly that one field read - the count pins suppression and
// extent-end in a single assertion. Cases whose binder escapes to the
// enclosing block (patterns, out vars, deconstructions) expect zero.
func TestCSharpBindingScopes_BindersShadowSameNamedField(t *testing.T) {
	// Every fixture reuses this helper type as the field's type.
	const bag = `    public sealed class ShBag : System.IDisposable {
        public void Touch() { }
        public int Weigh() { return 1; }
        public bool Alive() { return true; }
        public int Key() { return 2; }
        public void Dispose() { }
    }
`
	cases := map[string]struct {
		body  string // members of the ShFlow class (field + method)
		owner string // method under test
		name  string // colliding identifier
		reads int    // expected field reads from owner
	}{
		"foreach_identifier": {body: `        private ShBag _box = new ShBag();
        public void Go(ShBag[] bags) {
            foreach (var _box in bags) { _box.Touch(); }
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"foreach_tuple": {body: `        private ShBag _box = new ShBag();
        public void Go((int, ShBag)[] pairs) {
            foreach (var (_n, _box) in pairs) { _box.Touch(); _ = _n; }
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"lambda_implicit_param": {body: `        private ShBag _box = new ShBag();
        public void Go() {
            System.Func<ShBag, int> f = _box => _box.Weigh();
            _ = f;
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"lambda_typed_param_list": {body: `        private ShBag _box = new ShBag();
        public void Go() {
            System.Func<ShBag, int> f = (ShBag _box) => _box.Weigh();
            _ = f;
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"anonymous_method_param": {body: `        private ShBag _box = new ShBag();
        public void Go() {
            System.Func<ShBag, int> f = delegate(ShBag _box) { return _box.Weigh(); };
            _ = f;
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"local_function_param": {body: `        private ShBag _box = new ShBag();
        public void Go() {
            _box.Touch();
            int Helper(ShBag _box) { return _box.Weigh(); }
            _ = Helper(new ShBag());
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"catch_variable": {body: `        private ShBag _box = new ShBag();
        public void Go() {
            try { } catch (System.Exception _box) { _ = _box.ToString(); }
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"for_initializer": {body: `        private ShBag _box = new ShBag();
        public void Go() {
            for (var _box = new ShBag(); _box.Alive(); ) { _box.Touch(); break; }
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"using_resource": {body: `        private ShBag _box = new ShBag();
        public void Go() {
            using (var _box = new ShBag()) { _box.Touch(); }
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"declaration_pattern_escapes_to_block": {body: `        private ShBag _box = new ShBag();
        public void Go(object o) {
            if (!(o is ShBag _box)) { return; }
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 0},
		"var_pattern": {body: `        private ShBag _box = new ShBag();
        public void Go(object o) {
            if (o is var _box) { _ = _box.ToString(); }
        }`, owner: "ShFlow.Go", name: "_box", reads: 0},
		"out_var_declaration": {body: `        private ShBag _box = new ShBag();
        public void Go(System.Collections.Generic.Dictionary<int, ShBag> map) {
            if (map.TryGetValue(1, out var _box)) { _box.Touch(); }
        }`, owner: "ShFlow.Go", name: "_box", reads: 0},
		"recursive_pattern_designation": {body: `        private ShBag _box = new ShBag();
        public void Go(object o) {
            if (o is ShBag { } _box) { _box.Touch(); }
        }`, owner: "ShFlow.Go", name: "_box", reads: 0},
		"list_pattern_designation": {body: `        private ShBag[] _row = new ShBag[0];
        public void Go(ShBag[] xs) {
            if (xs is [_] _row) { _ = _row.Length; }
        }`, owner: "ShFlow.Go", name: "_row", reads: 0},
		"switch_case_var_designation": {body: `        private ShBag _box = new ShBag();
        public void Go((int, ShBag) pair) {
            switch (pair) {
                case var (_n, _box): _box.Touch(); _ = _n; break;
            }
        }`, owner: "ShFlow.Go", name: "_box", reads: 0},
		"deconstruction_declaration": {body: `        private ShBag _box = new ShBag();
        public void Go((int, ShBag) pair) {
            var (_n, _box) = pair;
            _box.Touch();
            _ = _n;
        }`, owner: "ShFlow.Go", name: "_box", reads: 0},
		"is_var_deconstruction_misparse": {body: `        private ShBag _box = new ShBag();
        public void Go((int, ShBag) pair) {
            if (pair is var (_n, _box)) { _box.Touch(); _ = _n; }
        }`, owner: "ShFlow.Go", name: "_box", reads: 0},
		"query_from_range_variable": {body: `        private ShBag _box = new ShBag();
        public void Go(ShBag[] bags) {
            var q = from _box in bags select _box.Weigh();
            _ = q;
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"query_let_variable": {body: `        private ShBag _box = new ShBag();
        public ShBag Make(int x) { return new ShBag(); }
        public void Go(int[] xs) {
            var q = from x in xs let _box = Make(x) select _box.Weigh();
            _ = q;
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"query_join_variable": {body: `        private ShBag _box = new ShBag();
        public void Go(int[] xs, ShBag[] bags) {
            var q = from x in xs join _box in bags on x equals _box.Key() select _box.Weigh();
            _ = q;
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
		"query_join_into_group": {body: `        private ShBag[] grp = new ShBag[0];
        public void Go(int[] xs, ShBag[] bags) {
            var q = from x in xs join b in bags on x equals b.Key() into grp select grp.Count();
            _ = q;
            _ = grp.Length;
        }`, owner: "ShFlow.Go", name: "grp", reads: 1},
		"query_continuation_variable": {body: `        private ShBag _box = new ShBag();
        public void Go(int[] xs) {
            var q = from x in xs select x into _box select _box.CompareTo(1);
            _ = q;
            _box.Touch();
        }`, owner: "ShFlow.Go", name: "_box", reads: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			src := "using System.Linq;\nnamespace App {\n" + bag +
				"    public sealed class ShFlow {\n" + tc.body + "\n    }\n}\n"
			res, err := NewCSharpExtractor().Extract("Sh.cs", []byte(src))
			require.NoError(t, err)
			assert.Equal(t, tc.reads, csharpFieldEdgeCount(res, "Sh.cs::"+tc.owner, tc.name, graph.EdgeReads),
				"the binder shadows the field inside its extent; a use outside it (when the case has one) is the field again")
		})
	}
}
