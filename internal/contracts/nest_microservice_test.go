package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func nestFindByID(cs []Contract, id string) *Contract {
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i]
		}
	}
	return nil
}

func nestMethodNode(file, name string, line int) *graph.Node {
	return &graph.Node{ID: file + "::" + name, Name: name, Kind: graph.KindMethod, FilePath: file, StartLine: line, EndLine: line + 2}
}

func TestMessagePatternHandlers(t *testing.T) {
	src := []byte(`import { MessagePattern, EventPattern } from '@nestjs/microservices';

@Controller()
export class MathController {
  @MessagePattern({ cmd: 'sum' })
  accumulate(data: number[]): number {
    return 0;
  }

  @EventPattern('user_created')
  handleUserCreated(data: any) {}
}
`)
	nodes := []*graph.Node{
		nestMethodNode("c.ts", "accumulate", 6),
		nestMethodNode("c.ts", "handleUserCreated", 10),
	}
	cs := (&NestMicroserviceExtractor{}).Extract("c.ts", src, nodes, nil)

	sum := nestFindByID(cs, "topic::sum")
	if sum == nil {
		t.Fatalf("expected topic::sum from @MessagePattern, got %+v", cs)
	}
	if sum.Type != ContractTopic || sum.Role != RoleProvider {
		t.Errorf("sum type/role = %v/%v", sum.Type, sum.Role)
	}
	if sum.Meta["message_kind"] != "MessagePattern" {
		t.Errorf("sum message_kind = %v", sum.Meta["message_kind"])
	}
	if sum.SymbolID != "c.ts::accumulate" {
		t.Errorf("sum handler = %q", sum.SymbolID)
	}

	evt := nestFindByID(cs, "topic::user_created")
	if evt == nil {
		t.Fatalf("expected topic::user_created from @EventPattern, got %+v", cs)
	}
	if evt.Meta["message_kind"] != "EventPattern" {
		t.Errorf("event message_kind = %v", evt.Meta["message_kind"])
	}
	if evt.SymbolID != "c.ts::handleUserCreated" {
		t.Errorf("event handler = %q", evt.SymbolID)
	}
}

func TestNestMicroservice_SupportedLanguages(t *testing.T) {
	langs := (&NestMicroserviceExtractor{}).SupportedLanguages()
	found := false
	for _, l := range langs {
		if l == "typescript" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected typescript in SupportedLanguages, got %v", langs)
	}
	// A file without the decorators yields nothing (prefilter).
	if cs := (&NestMicroserviceExtractor{}).Extract("x.ts", []byte("const a = 1\n"), nil, nil); len(cs) != 0 {
		t.Errorf("non-microservice file should produce no contracts, got %+v", cs)
	}
}

// TestNestCrossModulePrefixAndMessagePatterns proves the two NestJS depth
// upgrades. (1) RouterModule cross-module prefixing: a controller's route is
// prefixed by the path its module is mounted under in RouterModule.register —
// resolved by walking three separate files (router config, @Module, controller)
// plus the @Controller and child-route nesting, the multi-file reach a same-
// file regex cannot achieve. (2) @MessagePattern / @EventPattern handlers are
// first-class topic providers bound to their handler symbol, and @All routes
// are recognised.
func TestNestCrossModulePrefixAndMessagePatterns(t *testing.T) {
	t.Run("cross_module_prefix", func(t *testing.T) {
		files := map[string]string{
			"src/app.module.ts": `RouterModule.register([
  { path: 'admin', module: AdminModule, children: [
    { path: 'users', module: UsersModule },
  ] },
]);`,
			"src/users/users.module.ts": `@Module({ controllers: [UsersController] })
export class UsersModule {}`,
			"src/users/users.controller.ts": `@Controller('list')
export class UsersController {
  @Get('/active')
  active() {}
  @All('/any')
  any() {}
}`,
		}
		reg := NewRegistry()
		h := &HTTPExtractor{}
		for fp, src := range files {
			for _, c := range h.Extract(fp, []byte(src), nil, nil) {
				reg.Add(c)
			}
		}
		joinRouterFixtureFacts(t, reg, srcMap(files))

		got := map[string]bool{}
		for _, c := range reg.All() {
			if c.Type == ContractHTTP {
				m, _ := c.Meta["method"].(string)
				p, _ := c.Meta["path"].(string)
				got[m+" "+p] = true
			}
		}
		// admin (AdminModule) / users (UsersModule under admin's children) /
		// list (@Controller) / active (@Get) — composed across three files.
		if !got["GET /admin/users/list/active"] {
			t.Errorf("cross-module prefix not applied: want GET /admin/users/list/active, got %v", keysOfBool(got))
		}
		// @All is recognised (method ALL) and carries the same prefix.
		if !got["ALL /admin/users/list/any"] {
			t.Errorf("@All route missing or unprefixed, got %v", keysOfBool(got))
		}
	})

	t.Run("message_patterns", func(t *testing.T) {
		const fp = "users.controller.ts"
		src := `@Controller()
export class UsersController {
  @MessagePattern('user.find')
  find() {}

  @EventPattern('user.created')
  onCreated() {}
}`
		nodes := []*graph.Node{
			{ID: fp + "::UsersController.find", Kind: graph.KindMethod, Name: "find", FilePath: fp},
			{ID: fp + "::UsersController.onCreated", Kind: graph.KindMethod, Name: "onCreated", FilePath: fp},
		}
		cs := (&NestMicroserviceExtractor{}).Extract(fp, []byte(src), nodes, nil)
		find := nestFindByID(cs, "topic::user.find")
		if find == nil {
			t.Fatalf("missing topic::user.find contract, got %+v", cs)
		}
		if find.Meta["message_kind"] != "MessagePattern" {
			t.Errorf("message_kind=%v, want MessagePattern", find.Meta["message_kind"])
		}
		if find.SymbolID != fp+"::UsersController.find" {
			t.Errorf("@MessagePattern handler not bound: SymbolID=%q", find.SymbolID)
		}
		created := nestFindByID(cs, "topic::user.created")
		if created == nil || created.Meta["message_kind"] != "EventPattern" {
			t.Errorf("missing/incorrect topic::user.created (EventPattern), got %+v", created)
		}
		if created != nil && created.SymbolID != fp+"::UsersController.onCreated" {
			t.Errorf("@EventPattern handler not bound: SymbolID=%q", created.SymbolID)
		}
	})
}
