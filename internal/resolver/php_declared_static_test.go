package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestResolveAll_PHPDeclaredStatic(t *testing.T) {
	for _, tc := range []struct {
		name, namespace, imports, receiver, method, want string
		noMetadata, nonPHP                               bool
	}{
		{name: "declared make", imports: `use Acme\Values\Money;`, receiver: "Money", method: "make", want: "Money.make"},
		{name: "declared create", imports: `use Acme\Values\Money;`, receiver: "Money", method: "create", want: "Money.create"},
		{name: "declared query", imports: `use Acme\Values\Money;`, receiver: "Money", method: "query", want: "Money.query"},
		{name: "vendor import absent", imports: `use Illuminate\Support\Collection;`, receiver: "Collection", method: "make"},
		{name: "alias target absent", imports: `use Acme\Missing\Money as Cash;`, receiver: "Cash", method: "make"},
		{name: "alias beats unrelated class", imports: `use Acme\Values\Money as Cash;`, receiver: "Cash", method: "make", want: "Money.make"},
		{name: "group alias", imports: `use Acme\Values\{Money as Cash};`, receiver: "Cash", method: "make", want: "Money.make"},
		{name: "group import", imports: `use Acme\Values\{Money};`, receiver: "Money", method: "make", want: "Money.make"},
		{name: "qualified receiver", imports: `use Acme\Archive\Money;`, receiver: `\Acme\Values\Money`, method: "make", want: "Money.make"},
		{name: "qualified target absent", imports: `use Acme\Values\Money;`, receiver: `\Acme\Missing\Money`, method: "make"},
		{name: "same namespace", namespace: `Acme\Values`, receiver: "Money", method: "make", want: "Money.make"},
		{name: "unimported class", receiver: "Collection", method: "make"},
		{name: "finder unchanged", imports: `use Acme\Models\Order;`, receiver: "Order", method: "find"},
		{name: "no metadata", imports: `use Acme\Values\Money;`, receiver: "Money", method: "make", noMetadata: true},
		{name: "other language", imports: `use Acme\Values\Money;`, receiver: "Money", method: "make", nonPHP: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			sources := map[string]string{
				"app/Values/Money.php": `<?php namespace Acme\Values;
                    final class Money {
                        public static function make() {}
                        public static function create() {}
                        public static function query() {}
                    }`,
				"app/Archive/Money.php":      `<?php namespace Acme\Archive; class Money { public static function make() {} }`,
				"app/Legacy/Cash.php":        `<?php namespace Acme\Legacy; class Cash { public static function make() {} }`,
				"app/Support/Collection.php": `<?php namespace Acme\Support; class Collection { public static function make() {} }`,
				"app/Models/Order.php":       `<?php namespace Acme\Models; class Order {}`,
			}
			ns := tc.namespace
			if ns == "" {
				ns = `Acme\Services`
			}
			callerFile := "app/Services/Checkout.php"
			sources[callerFile] = fmt.Sprintf(`<?php namespace %s;
                %s
                class Checkout { public function total() { return %s::%s(); } }`, ns, tc.imports, tc.receiver, tc.method)
			var call *graph.Edge
			for path, src := range sources {
				result, err := languages.NewPHPExtractor().Extract(path, []byte(src))
				require.NoError(t, err)
				if result.Tree != nil {
					defer result.Tree.Close()
				}
				for _, n := range result.Nodes {
					n.RepoPrefix = "acme"
					if tc.nonPHP && n.FilePath == callerFile {
						n.Language = "javascript"
					}
					g.AddNode(n)
				}
				for _, e := range result.Edges {
					if path == callerFile && e.Kind == graph.EdgeCalls {
						call = e
						if tc.noMetadata {
							e.Meta = nil
						}
					}
					g.AddEdge(e)
				}
			}
			require.NotNil(t, call, "real extractor must emit the static call")
			New(g).ResolveAll()
			want := "unresolved::" + phpBaseTypeName(tc.receiver)
			if tc.want != "" {
				want = "app/Values/Money.php::" + tc.want
			}
			assert.Equal(t, want, call.To)
			if tc.want != "" {
				assert.Equal(t, graph.OriginASTResolved, call.Origin)
				assert.Equal(t, 0.85, call.Confidence)
			}
		})
	}
}
