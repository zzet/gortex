package tstypes

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

const tsSvc = `export class Svc {
  run(): void {
  }

  stop(): void {
  }
}
`

func TestTypeScript_DeclaredTypeResolvesCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"src/svc.ts": tsSvc,
		"src/app.ts": `import { Svc } from "./svc";

export class App {
  handle(s: Svc): void {
    s.run();
  }
}
`,
	})
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "handle", graph.KindMethod)
	target := nodeByNameKind(t, g, "run", graph.KindMethod)
	e := callEdgeTo(g, caller.ID, target.ID)
	if e == nil {
		t.Fatalf("annotated-param call not resolved; edges: %v", g.GetOutEdges(caller.ID))
	}
	assertASTProvenance(t, e, "typescript-types")
}

func TestTypeScript_ConstructorInferenceResolvesCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"src/svc.ts": tsSvc,
		"src/app.ts": `import { Svc } from "./svc";

export function main(): void {
  const s = new Svc();
  s.run();
}
`,
	})
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "main", graph.KindFunction)
	target := nodeByNameKind(t, g, "run", graph.KindMethod)
	if callEdgeTo(g, caller.ID, target.ID) == nil {
		t.Fatalf("constructor-inferred call not resolved; edges: %v", g.GetOutEdges(caller.ID))
	}
}

// Two same-named classes: the relative-import hint must pick the right
// one.
func TestTypeScript_ImportHintDisambiguates(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"src/svc.ts":   tsSvc,
		"other/svc.ts": tsSvc,
		"src/app.ts": `import { Svc } from "./svc";

export function main(): void {
  const s = new Svc();
  s.run();
}
`,
	})
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "main", graph.KindFunction)
	want := "src/svc.ts::Svc.run"
	if callEdgeTo(g, caller.ID, want) == nil {
		t.Fatalf("import-hinted call did not land on %s; edges: %v", want, g.GetOutEdges(caller.ID))
	}
	if callEdgeTo(g, caller.ID, "other/svc.ts::Svc.run") != nil {
		t.Fatal("call landed on the wrong module's class")
	}
}

func TestTypeScript_ImplementsAndExtendsSynthesis(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"src/svc.ts": tsSvc,
		"src/iface.ts": `export interface Greeter {
  greet(): void;
}
`,
		"src/impl.ts": `import { Greeter } from "./iface";
import { Svc } from "./svc";

export class Impl extends Svc implements Greeter {
  greet(): void {
  }
}
`,
	})
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	impl := nodeByNameKind(t, g, "Impl", graph.KindType)
	iface := nodeByNameKind(t, g, "Greeter", graph.KindInterface)
	svc := nodeByNameKind(t, g, "Svc", graph.KindType)
	if e := edgeBetween(g, impl.ID, graph.EdgeImplements, iface.ID); e == nil {
		t.Fatalf("implements edge missing; edges: %v", g.GetOutEdges(impl.ID))
	} else {
		assertASTProvenance(t, e, "typescript-types")
	}
	if e := edgeBetween(g, impl.ID, graph.EdgeExtends, svc.ID); e == nil {
		t.Fatalf("extends edge missing; edges: %v", g.GetOutEdges(impl.ID))
	} else {
		assertASTProvenance(t, e, "typescript-types")
	}
}

// this-qualified calls and typed class fields resolve inside a class.
func TestTypeScript_SelfAndFieldReceivers(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"src/svc.ts": tsSvc,
		"src/app.ts": `import { Svc } from "./svc";

export class App {
  private worker: Svc = new Svc();

  direct(): void {
    this.helper();
  }

  helper(): void {
    this.worker.run();
  }
}
`,
	})
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	direct := nodeByNameKind(t, g, "direct", graph.KindMethod)
	helper := nodeByNameKind(t, g, "helper", graph.KindMethod)
	if callEdgeTo(g, direct.ID, helper.ID) == nil {
		t.Fatalf("this.helper() not resolved; edges: %v", g.GetOutEdges(direct.ID))
	}
	run := nodeByNameKind(t, g, "run", graph.KindMethod)
	if callEdgeTo(g, helper.ID, run.ID) == nil {
		t.Fatalf("this.worker.run() not resolved; edges: %v", g.GetOutEdges(helper.ID))
	}
}

// JavaScript files (no annotations) still ground constructor-inferred
// receivers through the JS grammar.
func TestTypeScript_JavaScriptConstructorInference(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"src/svc.js": `export class Svc {
  run() {
  }
}
`,
		"src/app.js": `import { Svc } from "./svc";

export function main() {
  const s = new Svc();
  s.run();
}
`,
	})
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "main", graph.KindFunction)
	target := nodeByNameKind(t, g, "run", graph.KindMethod)
	if callEdgeTo(g, caller.ID, target.ID) == nil {
		t.Fatalf("JS constructor-inferred call not resolved; edges: %v", g.GetOutEdges(caller.ID))
	}
}

func TestTypeScript_AmbiguousReceiverStaysUntouched(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"src/svc.ts": tsSvc,
		"src/alt.ts": `export class Alt {
  run(): void {
  }
}
`,
		"src/app.ts": `import { Alt } from "./alt";
import { Svc } from "./svc";

export function main(): void {
  let s = new Svc();
  s = new Alt();
  s.run();
}
`,
	})
	p := NewProvider(TypeScriptSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	caller := nodeByNameKind(t, g, "main", graph.KindFunction)
	assertUntouched(t, g, caller.ID, "run", "typescript-types")
}
