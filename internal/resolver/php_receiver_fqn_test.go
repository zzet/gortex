package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestResolveAll_PHPReceiverFQN(t *testing.T) {
	for _, tc := range []struct {
		name, header, body, target string
		extra                      string
	}{
		{"new static receiver", `namespace Acme\Shop;`, `class Runner { static function run() { $w = new static; $w->handle(); } function handle() {} }`, "Runner.php::Runner.handle", ""},
		{"new self receiver", `namespace Acme\Shop;`, `class Runner { static function run() { $w = new self; $w->handle(); } function handle() {} }`, "Runner.php::Runner.handle", ""},
		{"imported parameter", `namespace Acme; use Acme\Shop\Widget;`, `function run(Widget $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"import case", `namespace Acme; use Acme\Shop\Widget;`, `function run(widget $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"alias case", `namespace Acme; use Acme\Shop\Widget as Panel;`, `function run(pAnEl $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"namespace alias case", `namespace Acme; use Acme\Shop as Store;`, `function run(sToRe\Widget $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"promoted property", `namespace Acme; use Acme\Shop\Widget;`, `class Runner { function __construct(private Widget $w) {} function run() { $this->w->handle(); } }`, "shop/Widget.php::Widget.handle", ""},
		{"static import", `namespace Acme; use Acme\Shop\Widget;`, `function run() { Widget::build(); }`, "shop/Widget.php::Widget.build", ""},
		{"alias parameter", `namespace Acme; use Acme\Shop\Widget as Panel;`, `function run(Panel $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"alias static", `namespace Acme; use Acme\Shop\Widget as Panel;`, `function run() { Panel::build(); }`, "shop/Widget.php::Widget.build", ""},
		{"absolute parameter", `namespace Acme;`, `function run(\Acme\Shop\Widget $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"absolute static", `namespace Acme;`, `function run() { \Acme\Shop\Widget::build(); }`, "shop/Widget.php::Widget.build", ""},
		{"absolute new", `namespace Acme;`, `function run() { $w = new \Acme\Shop\Widget; $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"namespace alias", `namespace Acme; use Acme\Shop as Store;`, `function run(Store\Widget $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"no import", `namespace Acme\Shop;`, `function run(Widget $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", ""},
		{"global no import", "", `function run(Panel $w) { $w->handle(); }`, "Panel.php::Panel.handle", ""},
		{"missing import target", `namespace Acme; use Acme\Missing\Widget;`, `function run(Widget $w) { $w->handle(); }`, "", ""},
		{"missing alias target", `namespace Acme; use Acme\Missing\Widget as Panel;`, `function run(Panel $w) { $w->handle(); }`, "", `namespace Acme\Shop; class Child extends Widget {}`},
		{"duplicate target", `namespace Acme; use Acme\Shop\Widget;`, `function run(Widget $w) { $w->handle(); }`, "", `namespace Acme\Shop; class Widget { function handle() {} static function build() {} }`},
		{"inherited method", `namespace Acme; use Acme\Shop\Child as Panel;`, `function run(Panel $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", `namespace Acme\Shop; class Child extends Widget {}`},
		{"missing method", `namespace Acme; use Acme\Shop\Child as Panel;`, `function run(Panel $w) { $w->handle(); }`, "", `namespace Acme\Shop; class Child {}`},
		{"missing absolute parent", `namespace Acme; use Acme\Shop\Child as Panel;`, `function run(Panel $w) { $w->handle(); }`, "", `namespace Acme\Shop; class Child extends \Widget {}`},
		{"ambiguous parent", `namespace Acme; use Acme\Shop\Child as Panel;`, `function run(Panel $w) { $w->handle(); }`, "", `namespace Acme\Shop; class Child extends Widget {} class Widget { function handle() {} static function build() {} }`},
		{"trait before unknown parent", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Handles.handle", `namespace Acme\Shop;
trait Handles { function handle() {} }
class Child extends \Acme\Vendor\Model { use Handles; }`},
		{"grandparent trait", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Handles.handle", `namespace Acme\Shop;
trait Handles { function handle() {} }
class Grandparent extends \Acme\Vendor\Model { use Handles; }
class ParentType extends Grandparent {}
class Child extends ParentType {}`},
		{"trait overrides parent", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Handles.handle", `namespace Acme\Shop;
trait Handles { function handle() {} }
class Child extends Widget { use Handles; }`},
		{"class overrides trait", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Child.handle", `namespace Acme\Shop;
trait Handles { function handle() {} }
class Child extends Widget { use Handles; function handle() {} }`},
		{"missing trait", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "", `namespace Acme\Shop; class Child extends Widget { use \Acme\Missing\Handles; }`},
		{"parent interface", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Contract.handle", `namespace Acme\Shop;
interface Contract { function handle(); }
class Grand {}
abstract class Mid extends Grand implements Contract {
}
class Child extends Mid {
}`},
		{"extended interface", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Contract.handle", `namespace Acme\Shop;
interface Contract { function handle(); }
interface Child extends Contract {
}`},
		{"unknown interface", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Handles.handle", `namespace Acme\Shop;
trait Handles { function handle() {} }
class Child extends Widget implements \Acme\Missing\Contract {
use Handles;
}`},
		{"parent before interface", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "shop/Widget.php::Widget.handle", `namespace Acme\Shop;
interface Contract { function handle(); }
class Child extends Widget implements Contract {
}`},
		{"nested trait precedence", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Outer.handle", `namespace Acme\Shop;
trait Inner { function handle() {} }
trait Outer { use Inner; function handle() {} }
class Child extends Widget {
use Outer;
}`},
		{"local trait beside vendor trait", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Handles.handle", `namespace Acme\Shop;
trait Handles { function handle() {} }
class Child extends \Acme\Vendor\Model { use \Acme\Vendor\Missing, Handles; }`},
		{"parent trait alias beside vendor trait", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Handles.handle", `namespace Acme\Shop;
use Acme\Shop\Handles as LocalHandlers;
trait Handles { function handle() {} }
class Base extends \Acme\Vendor\Model { use \Acme\Vendor\Missing, LocalHandlers; }
class Child extends Base {}`},
		{"nested trait beside vendor trait", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "Extra.php::Handles.handle", `namespace Acme\Shop;
trait Handles { function handle() {} }
trait Outer { use \Acme\Vendor\Missing, Handles; }
class Child extends Widget { use Outer; }`},
		{"partial trait collision", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "", `namespace Acme\Shop;
trait Handles { function handle() {} }
trait Other { function handle() {} }
class Child extends Widget { use \Acme\Vendor\Missing, Handles, Other; }`},
		{"unknown trait identity", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "", `namespace Acme\Shop;
trait Handles { function handle() {} }
class Child extends \Acme\Vendor\Model { use \Acme\Vendor\Handles; }`},
		{"conflicting traits", `namespace Acme; use Acme\Shop\Child;`, `function run(Child $w) { $w->handle(); }`, "", `namespace Acme\Shop;
trait Handles { function handle() {} }
trait Other { function handle() {} }
class Child extends Widget { use Handles, Other; }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			files := []struct{ path, source string }{
				{"archive/Runner.php", `namespace Acme\Archive; class Runner { function handle() {} }`},
				{"archive/Handles.php", `namespace Acme\Archive; trait Handles { function handle() {} }`},
				{"archive/Widget.php", `namespace Acme\Archive; class Widget { function handle() {} static function build() {} }`},
				{"Panel.php", `class Panel { function handle() {} static function build() {} }`},
				{"shop/Widget.php", `namespace Acme\Shop; class Widget { function handle() {} static function build() {} }`},
				{"Extra.php", tc.extra},
				{"Runner.php", tc.header + "\n" + tc.body},
			}
			for _, f := range files {
				res, err := languages.NewPHPExtractor().Extract(f.path, []byte("<?php\n"+strings.ReplaceAll(f.source, "{", "{\n")))
				require.NoError(t, err)
				for _, n := range res.Nodes {
					g.AddNode(n)
				}
				for _, e := range res.Edges {
					g.AddEdge(e)
				}
			}
			New(g).ResolveAll()
			New(g).ResolveAll() // A warm pass must preserve the same receiver identity.
			method := "handle"
			if strings.HasSuffix(tc.target, ".build") {
				method = "build"
			}
			var calls []*graph.Edge
			for e := range g.EdgesByKind(graph.EdgeCalls) {
				n := g.GetNode(e.To)
				if e.FilePath == "Runner.php" && (e.To == "unresolved::*."+method || (n != nil && n.Name == method)) {
					calls = append(calls, e)
				}
			}
			require.Len(t, calls, 1)
			if tc.target == "" {
				require.True(t, graph.IsUnresolvedTarget(calls[0].To), "unexpected target: %s", calls[0].To)
			} else {
				require.Equal(t, tc.target, calls[0].To)
			}
		})
	}
}

func TestPHPAncestorChain(t *testing.T) {
	res, err := languages.NewPHPExtractor().Extract("Types.php", []byte(`<?php
namespace Acme;
trait Handles {}
class Base {
use Handles;
}
class Child extends Base {
}
class ExternalChild extends \Acme\Vendor\Model {
use Handles;
}
class MissingTrait extends Base {
use \Acme\Missing\Handles;
}
`))
	require.NoError(t, err)
	g := graph.New()
	for _, n := range res.Nodes {
		g.AddNode(n)
	}
	for _, e := range res.Edges {
		g.AddEdge(e)
	}
	for _, tc := range []struct {
		name     string
		chain    []ancestorTier
		complete bool
	}{
		{"Child", []ancestorTier{{"class", []string{"Types.php::Child"}}, {"parent", []string{"Types.php::Base"}}, {"trait", []string{"Types.php::Handles"}}}, true},
		{"ExternalChild", []ancestorTier{{"class", []string{"Types.php::ExternalChild"}}, {"trait", []string{"Types.php::Handles"}}}, false},
		{"MissingTrait", []ancestorTier{{"class", []string{"Types.php::MissingTrait"}}}, false},
		{"Absent", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain, complete := New(g).phpAncestorChain("Types.php::" + tc.name)
			require.Equal(t, tc.chain, chain)
			require.Equal(t, tc.complete, complete)
			parent, ok := New(g).phpParentID("Types.php::" + tc.name)
			hasParent := tc.name == "Child" || tc.name == "MissingTrait"
			require.Equal(t, hasParent, ok)
			if hasParent {
				require.Equal(t, "Types.php::Base", parent)
			}
		})
	}
}
