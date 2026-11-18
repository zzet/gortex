package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestPHPTypeAliasCandidates(t *testing.T) {
	for _, typeOnly := range []bool{true, false} {
		for _, tc := range []struct {
			name, written, fqn, want string
		}{
			{"alias", "BaseRequest", `Acme\Http\Requests\Request`, "base"},
			{"alias collision", "Cash", `Acme\Values\Money`, "money"},
			{"external alias", "BaseRequest", `Vendor\Http\Request`, ""},
			{"external alias collision", "Cash", `Vendor\Values\Money`, ""},
			{"non-alias external", "Request", `Vendor\Http\Request`, "base"},
			{"no import", "Cash", "", "cash"},
			{"CSharp qualifier", "Cash", "Acme.Values.Money", "cash"},
		} {
			t.Run(tc.name+map[bool]string{true: "/type", false: "/type-or-function"}[typeOnly], func(t *testing.T) {
				g := graph.New()
				phpType(g, "base", "Request", map[string]any{MetaScopeNamespace: `Acme\Http\Requests`})
				phpType(g, "money", "Money", map[string]any{MetaScopeNamespace: `Acme\Values`})
				phpType(g, "cash", "Cash", map[string]any{MetaScopeNamespace: `Acme\Legacy`})
				e := &graph.Edge{To: "unresolved::" + tc.written, FilePath: "app/Report.php",
					Meta: map[string]any{"target_fqn": tc.fqn}}
				r, stats := New(g), &ResolveStats{}
				if typeOnly {
					r.resolveTypeRef(e, tc.written, stats)
				} else {
					r.resolveTypeOrFunc(e, tc.written, stats)
				}
				if tc.want == "" {
					assert.Equal(t, "unresolved::"+tc.written, e.To)
					assert.Equal(t, 1, stats.Unresolved)
				} else {
					assert.Equal(t, tc.want, e.To)
					assert.Equal(t, 1, stats.Resolved)
				}
			})
		}
	}
}

func TestPHPTypeAliasExtractAndResolve(t *testing.T) {
	g := graph.New()
	for path, src := range map[string]string{
		"app/Http/Requests/Request.php": `<?php namespace Acme\Http\Requests;
abstract class Request { public function actor(): string { return 'system'; } }`,
		"app/Http/Requests/Api/Request.php": `<?php namespace Acme\Http\Requests\Api;
use Acme\Http\Requests\Request as BaseRequest;
abstract class Request extends BaseRequest {}`,
		"app/Http/Requests/Api/StoreRequest.php": `<?php namespace Acme\Http\Requests\Api;
class StoreRequest extends Request { public function check() { return $this->actor(); } }`,
		"app/Contracts/Exportable.php": `<?php namespace Acme\Contracts; interface Exportable {}`,
		"app/Values/Money.php":         `<?php namespace Acme\Values; class Money {}`,
		"app/Legacy/Cash.php":          `<?php namespace Acme\Legacy; class Cash {}`,
		"app/Audit/Log.php": `<?php namespace Acme\Audit;
class Log { public function actor() {} public function seed() {} }`,
		"app/Testing/TestCase.php": `<?php namespace Acme\Testing;
class TestCase { public function seed() {} }`,
		"app/Testing/PlusCase.php": `<?php namespace Acme\Testing;
use Acme\Testing\TestCase as BaseCase; class PlusCase extends BaseCase {}`,
		"app/Testing/Feature/FeatureCase.php": `<?php namespace Acme\Testing\Feature;
use Acme\Testing\PlusCase;
class FeatureCase extends PlusCase { public function run() { $this->seed(); } }`,
		"app/Reports/Report.php": `<?php namespace Acme\Reports;
use Acme\Contracts\Exportable as ExportContract;
use Acme\Values\Money as Cash;
class Report implements ExportContract {
    public function export(Cash $total) { return new Cash(100); }
}`,
	} {
		result, err := languages.NewPHPExtractor().Extract(path, []byte(src))
		require.NoError(t, err)
		for _, n := range result.Nodes {
			g.AddNode(n)
		}
		for _, e := range result.Edges {
			g.AddEdge(e)
		}
	}
	New(g).ResolveAll()
	for _, tc := range []struct {
		from, to string
		kind     graph.EdgeKind
	}{
		{"app/Http/Requests/Api/Request.php::Request", "app/Http/Requests/Request.php::Request", graph.EdgeExtends},
		{"app/Reports/Report.php::Report", "app/Contracts/Exportable.php::Exportable", graph.EdgeImplements},
		{"app/Reports/Report.php::Report.export", "app/Values/Money.php::Money", graph.EdgeInstantiates},
		{"app/Reports/Report.php::Report.export", "app/Values/Money.php::Money", graph.EdgeTypedAs},
		{"app/Http/Requests/Api/StoreRequest.php::StoreRequest.check", "app/Http/Requests/Request.php::Request.actor", graph.EdgeCalls},
		{"app/Testing/Feature/FeatureCase.php::FeatureCase.run", "app/Testing/TestCase.php::TestCase.seed", graph.EdgeCalls},
	} {
		var targets []string
		for _, e := range g.GetOutEdges(tc.from) {
			if e.Kind == tc.kind {
				targets = append(targets, e.To)
			}
		}
		assert.Contains(t, targets, tc.to, "%s from %s", tc.kind, tc.from)
		assert.NotContains(t, targets, "app/Legacy/Cash.php::Cash")
		assert.NotContains(t, targets, "app/Audit/Log.php::Log.actor")
		assert.NotContains(t, targets, "app/Audit/Log.php::Log.seed")
	}
}
