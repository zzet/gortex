package contracts

import (
	"testing"
)

// srcMap drives JoinRouterPrefixes directly: a file-path -> source map
// standing in for the indexer's disk reader. The pass is exercised
// without any graph / disk dependency.
type srcMap map[string]string

func (m srcMap) reader() func(string) []byte {
	return func(p string) []byte {
		s, ok := m[p]
		if !ok {
			return nil
		}
		return []byte(s)
	}
}

// paths returns every file path in the map — the scan-file universe
// JoinRouterPrefixes consumes (mount files included, which carry no
// route contracts).
func (m srcMap) paths() []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	return out
}

// extractInto runs the HTTP extractor over each file in the map and adds
// every contract to reg with the given repo/workspace scope, so the
// contracts carry real framework meta and normalised paths.
func extractInto(t *testing.T, reg *Registry, files srcMap, repo, ws string) {
	t.Helper()
	ext := &HTTPExtractor{}
	for path, src := range files {
		for _, c := range ext.Extract(path, []byte(src), nil, nil) {
			c.RepoPrefix = repo
			c.WorkspaceID = ws
			c.ProjectID = ws
			reg.Add(c)
		}
	}
}

func idSet(reg *Registry) map[string]bool {
	out := map[string]bool{}
	for _, c := range reg.All() {
		out[c.ID] = true
	}
	return out
}

// TestJoinRouterPrefixes_FastAPI_CrossFile is the primary target: a
// router declared with its own prefix in users.py, mounted under a
// second prefix in main.py. The route @router.get("/{id}") must land at
// http::GET::/api/users/{p1}.
func TestJoinRouterPrefixes_FastAPI_CrossFile(t *testing.T) {
	files := srcMap{
		"users.py": `
from fastapi import APIRouter

router = APIRouter(prefix="/users")

@router.get("/{id}")
def get_user(id: int):
    return id

@router.get("/")
def list_users():
    return []
`,
		"main.py": `
from fastapi import FastAPI
from .users import router

app = FastAPI()
app.include_router(router, prefix="/api")
`,
	}

	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/api/users/{p1}"] {
		t.Errorf("expected joined provider http::GET::/api/users/{p1}; got %v", keysOfBool(ids))
	}
	if !ids["http::GET::/api/users"] {
		t.Errorf("expected joined provider http::GET::/api/users (from /); got %v", keysOfBool(ids))
	}
	// The un-joined originals must be gone.
	if ids["http::GET::/{p1}"] {
		t.Errorf("un-joined original http::GET::/{p1} still present: %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_FastAPI_Idempotent runs the pass twice; the
// second run must be a no-op (no double-join to /api/api/users).
func TestJoinRouterPrefixes_FastAPI_Idempotent(t *testing.T) {
	files := srcMap{
		"users.py": `router = APIRouter(prefix="/users")

@router.get("/{id}")
def get_user(id): return id
`,
		"main.py": `app.include_router(router, prefix="/api")`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)
	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/api/users/{p1}"] {
		t.Errorf("expected http::GET::/api/users/{p1} after double run; got %v", keysOfBool(ids))
	}
	if ids["http::GET::/api/api/users/{p1}"] {
		t.Errorf("double-join detected: %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_FastAPI_NestedIncludes chains prefixes: a
// sub-router is included into a parent router, which is itself included
// into the app. /v2 (app mount) + /admin (parent self) ... actually the
// chain is app.include_router(parent, prefix="/api") and
// parent.include_router(child, prefix="/admin"), child=APIRouter(prefix="/users").
// Route @child.get("/{id}") -> /api/admin/users/{id}.
func TestJoinRouterPrefixes_FastAPI_NestedIncludes(t *testing.T) {
	files := srcMap{
		"users.py": `child = APIRouter(prefix="/users")

@child.get("/{id}")
def get_user(id): return id
`,
		"admin.py": `parent = APIRouter()
parent.include_router(child, prefix="/admin")
`,
		"main.py": `app.include_router(parent, prefix="/api")`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/api/admin/users/{p1}"] {
		t.Errorf("expected chained http::GET::/api/admin/users/{p1}; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_FastAPI_NoPrefix: a router with no self-prefix
// mounted with no prefix is a no-op join — the ID stays as declared.
func TestJoinRouterPrefixes_FastAPI_NoPrefix(t *testing.T) {
	files := srcMap{
		"users.py": `router = APIRouter()

@router.get("/users/{id}")
def get_user(id): return id
`,
		"main.py": `app.include_router(router)`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/users/{p1}"] {
		t.Errorf("no-prefix join should leave http::GET::/users/{p1}; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_FastAPI_SelfPrefixOnly: router carries a
// prefix but is never mounted under another (or mounted with no
// prefix). The self-prefix still applies.
func TestJoinRouterPrefixes_FastAPI_SelfPrefixOnly(t *testing.T) {
	files := srcMap{
		"users.py": `router = APIRouter(prefix="/users")

@router.get("/{id}")
def get_user(id): return id
`,
		"main.py": `app.include_router(router)`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/users/{p1}"] {
		t.Errorf("self-prefix-only should yield http::GET::/users/{p1}; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_ConsumerPairsViaMatch is the end-to-end pin:
// after the join, a consumer calling /api/users/42 (full path) pairs
// with the rewritten provider via Match.
func TestJoinRouterPrefixes_ConsumerPairsViaMatch(t *testing.T) {
	providerFiles := srcMap{
		"users.py": `router = APIRouter(prefix="/users")

@router.get("/{id}")
def get_user(id): return id
`,
		"main.py": `app.include_router(router, prefix="/api")`,
	}
	reg := NewRegistry()
	extractInto(t, reg, providerFiles, "api", "shop")

	// Consumer in a sibling repo of the same workspace calls the full
	// joined path. Consumer paths are never prefix-joined (they already
	// carry the mount prefix), so this is added as-is.
	consumerSrc := `async function getUser(id) {
  return fetch(` + "`/api/users/${id}`" + `);
}
`
	ext := &HTTPExtractor{}
	for _, c := range ext.Extract("api.ts", []byte(consumerSrc), nil, nil) {
		c.RepoPrefix = "web"
		c.WorkspaceID = "shop"
		c.ProjectID = "shop"
		reg.Add(c)
	}

	joinRouterFixtureFacts(t, reg, providerFiles)

	result := Match(reg)
	var paired *CrossLink
	for i, m := range result.Matched {
		if m.ContractID == "http::GET::/api/users/{p1}" {
			paired = &result.Matched[i]
			break
		}
	}
	if paired == nil {
		t.Fatalf("expected http::GET::/api/users/{p1} pair; matched=%d orphan_prov=%d orphan_cons=%d",
			len(result.Matched), len(result.OrphanProviders), len(result.OrphanConsumers))
	}
	if !paired.CrossRepo {
		t.Errorf("expected CrossRepo=true (provider api, consumer web)")
	}
	if paired.Provider.RepoPrefix != "api" || paired.Consumer.RepoPrefix != "web" {
		t.Errorf("wrong wiring: provider=%s consumer=%s",
			paired.Provider.RepoPrefix, paired.Consumer.RepoPrefix)
	}
}

// TestJoinRouterPrefixes_Express joins an app.use('/api', router) mount
// onto a route declared on that router in a sibling file.
func TestJoinRouterPrefixes_Express(t *testing.T) {
	files := srcMap{
		"routes.ts": `
const router = express.Router();
router.get('/users/:id', getUser);
`,
		"app.ts": `
const app = express();
app.use('/api', router);
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/api/users/{p1}"] {
		t.Errorf("expected express-joined http::GET::/api/users/{p1}; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_NestJS joins a @Controller('cats') class
// prefix onto the class's @Get(':id') method route (single file).
func TestJoinRouterPrefixes_NestJS(t *testing.T) {
	files := srcMap{
		"cats.controller.ts": `
@Controller('cats')
export class CatsController {
  @Get(':id')
  findOne(@Param('id') id: string) {
    return id;
  }

  @Post('bulk')
  createBulk() {
    return 'created';
  }
}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/cats/{p1}"] {
		t.Errorf("expected nestjs-joined http::GET::/cats/{p1}; got %v", keysOfBool(ids))
	}
	if !ids["http::POST::/cats/bulk"] {
		t.Errorf("expected nestjs-joined http::POST::/cats/bulk; got %v", keysOfBool(ids))
	}
}

// keysOfBool returns the keys of a string->bool set for diagnostics.
func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestJoinRouterPrefixes_Flask_Blueprint joins a Blueprint url_prefix onto a
// @bp.route declared on that blueprint.
func TestJoinRouterPrefixes_Flask_Blueprint(t *testing.T) {
	files := srcMap{
		"api.py": `
from flask import Blueprint
bp = Blueprint('api', __name__, url_prefix='/api')

@bp.route('/users')
def list_users():
    return []
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/api/users"] {
		t.Errorf("expected flask-joined http::GET::/api/users; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_Flask_RegisterBlueprint joins a url_prefix supplied at
// register_blueprint() time across files.
func TestJoinRouterPrefixes_Flask_RegisterBlueprint(t *testing.T) {
	files := srcMap{
		"admin.py": `
from flask import Blueprint
admin = Blueprint('admin', __name__)

@admin.route('/dashboard')
def dashboard():
    return 'ok'
`,
		"app.py": `
from flask import Flask
from admin import admin
app = Flask(__name__)
app.register_blueprint(admin, url_prefix='/admin')
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")

	joinRouterFixtureFacts(t, reg, files)

	ids := idSet(reg)
	if !ids["http::GET::/admin/dashboard"] {
		t.Errorf("expected flask-joined http::GET::/admin/dashboard; got %v", keysOfBool(ids))
	}
}

func TestJoinRouterPrefixes_Gin(t *testing.T) {
	files := srcMap{
		"main.go": `package main
func setup(r *gin.Engine) {
	v1 := r.Group("/api/v1")
	v1.GET("/users", listUsers)
	admin := v1.Group("/admin")
	admin.POST("/ban", banUser)
}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/api/v1/users"] {
		t.Errorf("expected gin-joined http::GET::/api/v1/users; got %v", keysOfBool(ids))
	}
	if !ids["http::POST::/api/v1/admin/ban"] {
		t.Errorf("expected nested gin-joined http::POST::/api/v1/admin/ban; got %v", keysOfBool(ids))
	}
}

func TestJoinRouterPrefixes_Spring(t *testing.T) {
	files := srcMap{
		"UserController.java": `
@RestController
@RequestMapping("/api")
public class UserController {
	@GetMapping("/users")
	public List<User> list() { return null; }
}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/api/users"] {
		t.Errorf("expected spring-joined http::GET::/api/users; got %v", keysOfBool(ids))
	}
}

func TestJoinRouterPrefixes_Rails(t *testing.T) {
	files := srcMap{
		"routes.rb": `
Rails.application.routes.draw do
  namespace :admin do
    get '/users', to: 'users#index'
  end
end
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/admin/users"] {
		t.Errorf("expected rails-joined http::GET::/admin/users; got %v", keysOfBool(ids))
	}
}

func TestJoinRouterPrefixes_Laravel(t *testing.T) {
	files := srcMap{
		"routes.php": `<?php
Route::prefix('admin')->group(function () {
    Route::get('/users', [UserController::class, 'index']);
});
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/admin/users"] {
		t.Errorf("expected laravel-joined http::GET::/admin/users; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_VaporGrouped joins the assignment route-group form
// `let api = app.grouped("api")` onto a route declared on that group var.
func TestJoinRouterPrefixes_VaporGrouped(t *testing.T) {
	files := srcMap{
		"routes.swift": `
func routes(_ app: Application) throws {
    let api = app.grouped("api")
    api.get("users") { req in "ok" }
}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/api/users"] {
		t.Errorf("expected vapor-joined http::GET::/api/users; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_VaporClosureGroup joins the closure-block route-group
// form `app.group("api") { ... }` onto a route declared inside the brace block.
func TestJoinRouterPrefixes_VaporClosureGroup(t *testing.T) {
	files := srcMap{
		"routes.swift": `
func routes(_ app: Application) throws {
    app.group("api") {
        app.get("users") { req in "ok" }
    }
}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/api/users"] {
		t.Errorf("expected vapor closure-joined http::GET::/api/users; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_VaporClosureGroupNested chains nested closure groups
// and leaves a sibling route outside the block unprefixed.
func TestJoinRouterPrefixes_VaporClosureGroupNested(t *testing.T) {
	files := srcMap{
		"routes.swift": `
func routes(_ app: Application) throws {
    routes.group("v1") { g in
        g.group("admin") { a in
            a.post("ban") { req in "ok" }
        }
    }
    app.get("health") { req in "ok" }
}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::POST::/v1/admin/ban"] {
		t.Errorf("expected nested vapor closure-joined http::POST::/v1/admin/ban; got %v", keysOfBool(ids))
	}
	if !ids["http::GET::/health"] {
		t.Errorf("expected unscoped route to stay http::GET::/health; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_ActixCrossFileScope joins a web::scope("/api")
// .configure(api_routes) mount in main.rs onto a resource the api_routes
// config function registers in a sibling file.
func TestJoinRouterPrefixes_ActixCrossFileScope(t *testing.T) {
	files := srcMap{
		"main.rs": `
fn app() {
    App::new().service(web::scope("/api").configure(api_routes));
}
`,
		"api.rs": `
fn api_routes(cfg: &mut web::ServiceConfig) {
    cfg.service(web::resource("/users").route(web::get().to(list)));
}
async fn list() {}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/api/users"] {
		t.Errorf("expected actix cross-file-scope-joined http::GET::/api/users; got %v", keysOfBool(ids))
	}
}

// TestJoinRouterPrefixes_ActixCrossFileScopeService joins a web::scope mounted
// via .service(<path::fn>), confirming the path-qualified config reference is
// keyed on its final segment.
func TestJoinRouterPrefixes_ActixCrossFileScopeService(t *testing.T) {
	files := srcMap{
		"main.rs": `
fn app() {
    App::new().service(web::scope("/v2").service(users::routes));
}
`,
		"users.rs": `
fn routes(cfg: &mut web::ServiceConfig) {
    cfg.service(web::resource("/me").route(web::get().to(me)));
}
async fn me() {}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/v2/me"] {
		t.Errorf("expected actix cross-file-scope-joined http::GET::/v2/me; got %v", keysOfBool(ids))
	}
}

func TestJoinRouterPrefixes_Axum(t *testing.T) {
	files := srcMap{
		"main.rs": `
fn app() -> Router {
    let api = Router::new().route("/users", get(list_users));
    Router::new().nest("/api", api)
}
`,
	}
	reg := NewRegistry()
	extractInto(t, reg, files, "svc", "svc")
	joinRouterFixtureFacts(t, reg, files)
	ids := idSet(reg)
	if !ids["http::GET::/api/users"] {
		t.Errorf("expected axum-joined http::GET::/api/users; got %v", keysOfBool(ids))
	}
}
