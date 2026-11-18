package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestResolveAll_PHPPropertyReceiver(t *testing.T) {
	for _, tc := range []struct {
		name, imports, declaration, body, field, want, members string
		heritage, baseField                                    string
		duplicate, family, interfaceType                       bool
		confidence                                             float64
	}{
		{name: "untyped root fallback", declaration: "$event", body: "$event->mailer->dispatchMail()", want: "Mailer.php::Mailer.dispatchMail", confidence: 0.7},
		{name: "untyped constructor property fallback", body: "$this->mailer->dispatchMail()", members: `private $mailer; public function __construct(\Acme\Mailer $mailer) { $this->mailer = $mailer; }`, want: "Mailer.php::Mailer.dispatchMail", confidence: 0.7},
		{name: "untyped root dispatch", declaration: "$event", body: "$event->mailer->dispatchMail()", family: true, want: "Mailer.php::Mailer.dispatchMail"},
		{name: "unknown type blocks dispatch", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->dispatchMail()", field: "public Missing $widget;", family: true},
		{name: "parameter", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", field: "public Widget $widget;", want: "Widget.php::Widget.render"},
		{name: "interface property", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", field: "public Widget $widget;", interfaceType: true, want: "Widget.php::Widget.render"},
		{name: "property alias", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", field: "public Panel $widget;", want: "Widget.php::Widget.render"},
		{name: "qualified parameter", declaration: `\Acme\Parcel $parcel`, body: "$parcel->widget->render()", field: "public Widget $widget;", want: "Widget.php::Widget.render"},
		{name: "inherited", imports: `use Acme\Base;`, body: "$this->widget->render()", field: "public function __construct(protected Widget $widget) {}", want: "Widget.php::Widget.render"},
		{name: "trait before parent", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", field: "use Slot;", heritage: "extends Base", baseField: "public Widget $widget;", want: "Widget.php::Widget.render"},
		{name: "trait before unknown parent", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", field: "use Slot;", heritage: "extends Missing", want: "Widget.php::Widget.render"},
		{name: "unknown parent blocks fallback", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", heritage: "extends Missing"},
		{name: "no import", declaration: "Parcel $parcel", body: "$parcel->widget->render()", field: "public Widget $widget;"},
		{name: "missing property", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->absent->render()", field: "public Widget $widget;", want: "Runner.php::Runner.render", confidence: 0.9},
		{name: "unknown type", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", field: "public Missing $widget;"},
		{name: "union root", imports: `use Acme\Parcel as Package;`, declaration: "Package|Missing $parcel", body: "$parcel->widget->render()", field: "public Widget $widget;"},
		{name: "intersection root", imports: `use Acme\Parcel as Package;`, declaration: "Package&Missing $parcel", body: "$parcel->widget->render()", field: "public Widget $widget;"},
		{name: "union", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", field: "public Widget|Missing $widget;"},
		{name: "private inherited", imports: `use Acme\Base;`, body: "$this->widget->render()", field: "private Widget $widget;"},
		{name: "duplicate type", imports: `use Acme\Parcel as Package;`, declaration: "Package $parcel", body: "$parcel->widget->render()", field: "public Widget $widget;", duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			baseField := tc.field
			if tc.baseField != "" {
				baseField = tc.baseField
			}
			sources := map[string]string{
				"Mailer.php":       `<?php namespace Acme; class Mailer { public function dispatchMail(): void {} }`,
				"Slot.php":         `<?php namespace Acme; trait Slot { public Widget $widget; }`,
				"Widget.php":       `<?php namespace Acme; class Widget { public function render(): void {} }`,
				"other/Parcel.php": `<?php namespace Other; class Parcel { public Widget $widget; }`,
				"other/Widget.php": `<?php namespace Other; class Widget { public function render(): void {} }`,
				"Parcel.php":       `<?php namespace Acme; use Acme\Widget as Panel; class Parcel ` + tc.heritage + ` { ` + tc.field + ` }`,
				"Base.php":         `<?php namespace Acme; use Acme\Widget; class Base { ` + baseField + ` }`,
				"Runner.php": `<?php namespace Client; ` + tc.imports + ` class Runner extends Base { ` + tc.members + `
public function render(): void {}
public function run(` + tc.declaration + `): void { ` + tc.body + `; }
}`,
			}
			if tc.interfaceType {
				sources["Widget.php"] = `<?php namespace Acme; interface Widget { public function render(): void; }`
			}
			if tc.duplicate {
				sources["duplicate/Widget.php"] = sources["Widget.php"]
			}
			if tc.family {
				sources["Courier.php"] = `<?php namespace Acme; class Courier extends Mailer { public function dispatchMail(): void {} }`
			}
			for path, src := range sources {
				res, err := languages.NewPHPExtractor().Extract(path, []byte(src))
				require.NoError(t, err)
				for _, n := range res.Nodes {
					g.AddNode(n)
				}
				for _, e := range res.Edges {
					g.AddEdge(e)
				}
			}
			New(g).ResolveAll()
			var calls []*graph.Edge
			for _, e := range g.GetOutEdges("Runner.php::Runner.run") {
				if e.Kind == graph.EdgeCalls {
					calls = append(calls, e)
				}
			}
			if tc.family && tc.want != "" {
				require.Len(t, calls, 2)
				assert.ElementsMatch(t, []string{"Mailer.php::Mailer.dispatchMail", "Courier.php::Courier.dispatchMail"}, []string{calls[0].To, calls[1].To})
				return
			}
			require.Len(t, calls, 1)
			if tc.want == "" {
				assert.True(t, graph.IsUnresolvedTarget(calls[0].To), "unexpected target %s", calls[0].To)
			} else {
				assert.Equal(t, tc.want, calls[0].To)
				if tc.confidence != 0 {
					assert.InDelta(t, tc.confidence, calls[0].Confidence, 0.001)
				}
			}
		})
	}
}
