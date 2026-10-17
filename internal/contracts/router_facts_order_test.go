package contracts

import (
	"reflect"
	"sort"
	"testing"
)

// Stable path precedence is independent of the pure-facts split. Parent and
// controller ownership keep the first sorted file; module mounts keep the last.
func TestRouterFactsAmbiguousNamesHaveStablePathPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		files srcMap
		want  string
	}{
		{"parent_first_wins", srcMap{
			"a.py":      "alpha = APIRouter(prefix='/alpha')\nalpha.include_router(router)",
			"z.py":      "omega = APIRouter(prefix='/omega')\nomega.include_router(router)",
			"routes.py": "router = APIRouter()\n@router.get('/users')\ndef users(): pass",
		}, "http::GET::/alpha/users"},
		{"controller_owner_first_wins", srcMap{
			"a.module.ts":         "@Module({ controllers: [UsersController] })\nexport class AlphaModule {}",
			"z.module.ts":         "@Module({ controllers: [UsersController] })\nexport class OmegaModule {}",
			"router.ts":           "RouterModule.register([{path:'alpha',module:AlphaModule},{path:'omega',module:OmegaModule}]);",
			"users.controller.ts": "@Controller('users')\nexport class UsersController {\n@Get('/active')\nactive() {}\n}",
		}, "http::GET::/alpha/users/active"},
		{"module_mount_last_wins", srcMap{
			"a.router.ts":         "RouterModule.register([{path:'alpha',module:UsersModule}]);",
			"z.router.ts":         "RouterModule.register([{path:'omega',module:UsersModule}]);",
			"users.module.ts":     "@Module({ controllers: [UsersController] })\nexport class UsersModule {}",
			"users.controller.ts": "@Controller('users')\nexport class UsersController {\n@Get('/active')\nactive() {}\n}",
		}, "http::GET::/omega/users/active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var expected []string
			paths := tc.files.paths()
			sort.Strings(paths)
			for iteration := 0; iteration < 20; iteration++ {
				facts := map[string]RouterFacts{}
				for step := range paths {
					idx := step
					if iteration%2 == 1 {
						idx = len(paths) - 1 - step
					}
					path := paths[idx]
					facts[path] = RouterFileFacts(path, []byte(tc.files[path]))
				}
				reg := NewRegistry()
				extractInto(t, reg, tc.files, "repo", "ws")
				JoinRouterPrefixFacts(reg, facts)
				if !idSet(reg)[tc.want] {
					t.Fatalf("unstable precedence: want %s got %v", tc.want, idSet(reg))
				}
				snapshot := routerContractSnapshot(t, reg)
				if iteration == 0 {
					expected = snapshot
				} else if !reflect.DeepEqual(expected, snapshot) {
					t.Fatal("insertion order changed join")
				}
			}
		})
	}
}
