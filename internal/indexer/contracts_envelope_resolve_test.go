package indexer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestContracts_ResponseFromMakeSlice reproduces the user's
// `result := make([]toolInfo, 0, len(tools))` case: a top-level
// response value bound from a builtin (make), not a method call.
// traceVarTypeFromBody can't resolve `make` (no graph node for the
// builtin), so we fall back to bindLiteralTypeFromBody, which knows
// `make([]T, …)` produces T-slice. After this pass the contract
// carries response_type="<repo>/handler.go::toolInfo",
// response_repeated=true, and response_shape populated with
// toolInfo's fields — the dashboard renders the response as
// `[{name: string, description: string}]`.
func TestContracts_ResponseFromMakeSlice(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "handler.go"), []byte(`package main

import "net/http"

type toolInfo struct {
	Name        string `+"`json:\"name\"`"+`
	Description string `+"`json:\"description\"`"+`
}

type Handler struct{ tools map[string]any }

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/tools", h.handleListTools)
}

func (h *Handler) handleListTools(w http.ResponseWriter, _ *http.Request) {
	result := make([]toolInfo, 0, len(h.tools))
	WriteJSON(w, http.StatusOK, result)
}

func WriteJSON(w http.ResponseWriter, code int, body any) {}
`), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	cr := idx.ContractRegistry()
	require.NotNil(t, cr)

	var found contracts.Contract
	for _, c := range cr.All() {
		if c.Type == contracts.ContractHTTP && strings.Contains(c.ID, "/v1/tools") {
			found = c
			break
		}
	}
	require.NotEmpty(t, found.ID, "expected HTTP contract for /v1/tools")

	rt, _ := found.Meta["response_type"].(string)
	if !strings.Contains(rt, "toolInfo") {
		t.Errorf("response_type = %q, want a toolInfo reference (resolved via make-slice fallback)", rt)
	}
	if r, _ := found.Meta["response_repeated"].(bool); !r {
		t.Errorf("response_repeated = false, want true (declared as []toolInfo)")
	}
	if got, _ := found.Meta["schema_source"].(string); got != "extracted" {
		t.Errorf("schema_source = %q, want \"extracted\"", got)
	}

	// And response_shape must be inlined from the toolInfo type node.
	shape := found.Meta["response_shape"]
	if shape == nil {
		t.Fatalf("response_shape missing; want toolInfo's struct fields inlined")
	}
	var fields []contracts.ShapeField
	switch s := shape.(type) {
	case *contracts.Shape:
		fields = s.Fields
	case contracts.Shape:
		fields = s.Fields
	}
	if len(fields) != 2 {
		t.Errorf("response_shape has %d fields, want 2 (Name, Description)", len(fields))
	}

	// The ugly response_expr ("WriteJSON(...)") must be gone — once
	// response_type resolves we delete the expr placeholder.
	if got, _ := found.Meta["response_expr"].(string); got != "" {
		t.Errorf("response_expr should be empty when response_type resolves; got %q", got)
	}
}

// TestContracts_HandlerHintsStripped asserts that the internal
// handler_ident / handler_trail extractor scratchpad never makes it
// onto the final wire shape, regardless of whether cross-file handler
// resolution succeeded. A user looking at the dashboard's location
// meta should never see fragments like `handler_trail: "/users", listUsers"`
// — those are extractor-internal hints, not contract data.
func TestContracts_HandlerHintsStripped(t *testing.T) {
	dir := t.TempDir()
	// Route registration referencing a handler that's NOT defined in
	// this repo — the cross-file resolver will give up, and previously
	// the unresolved hints would leak through to the wire. With the
	// unconditional cleanup the hints are gone either way.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "net/http"

func register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/external", externalHandler)
}
`), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	cr := idx.ContractRegistry()
	require.NotNil(t, cr)

	for _, c := range cr.All() {
		if c.Type != contracts.ContractHTTP {
			continue
		}
		if _, leaked := c.Meta["handler_ident"]; leaked {
			t.Errorf("contract %s leaks handler_ident: %v", c.ID, c.Meta["handler_ident"])
		}
		if _, leaked := c.Meta["handler_trail"]; leaked {
			t.Errorf("contract %s leaks handler_trail: %v", c.ID, c.Meta["handler_trail"])
		}
	}
}

// TestContracts_EnvelopeBuiltinBindings reproduces the user-reported
// "data doesn't match" case: an envelope whose values come from
// non-traceable bindings (`r.PathValue("ws")`, `make([]string, ...)`).
// Both rows must end up typed via the literal-binding fallback so the
// dashboard renders `workspace: string` and `repos: [string]` instead
// of empty fields.
func TestContracts_EnvelopeBuiltinBindings(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "handler.go"), []byte(`package main

import "net/http"

type Handler struct{}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{ws}/repos", h.handleWorkspaceRoster)
}

func (h *Handler) handleWorkspaceRoster(w http.ResponseWriter, r *http.Request) {
	ws := r.PathValue("ws")
	repos := make([]string, 0, 4)
	WriteJSON(w, http.StatusOK, map[string]any{"workspace": ws, "repos": repos})
}

func WriteJSON(w http.ResponseWriter, code int, body any) {}
`), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	cr := idx.ContractRegistry()
	require.NotNil(t, cr)

	var found contracts.Contract
	var ok bool
	for _, c := range cr.All() {
		if c.Type == contracts.ContractHTTP && strings.Contains(c.ID, "/v1/workspaces") {
			found, ok = c, true
			break
		}
	}
	require.True(t, ok)

	envRaw, ok := found.Meta["response_envelope"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, envRaw, 2)
	byName := map[string]map[string]any{}
	for _, row := range envRaw {
		if name, _ := row["name"].(string); name != "" {
			byName[name] = row
		}
	}

	if got, _ := byName["workspace"]["type"].(string); got != "string" {
		t.Errorf("envelope[workspace].type = %q, want \"string\" (from r.PathValue)", got)
	}
	if got, _ := byName["repos"]["type"].(string); got != "string" {
		t.Errorf("envelope[repos].type = %q, want \"string\" (slice element from make([]string, ...))", got)
	}
	if r, _ := byName["repos"]["repeated"].(bool); !r {
		t.Errorf("envelope[repos].repeated = false, want true (declared as []string)")
	}
	if r, _ := byName["workspace"]["repeated"].(bool); r {
		t.Errorf("envelope[workspace].repeated = true, want false (scalar string)")
	}
}

// TestContracts_ResolveEnvelopeFieldTypes asserts that the indexer's
// graph-aware post-pass walks every response_envelope row whose type
// is empty and traces its expression back to the bound method's
// return type. The dashboard then renders `repos: []Repo` instead of
// the bare `repos` identifier — the user-facing motivation is "what
// does `repos` mean in `map[string]any{"repos": repos}`?".
func TestContracts_ResolveEnvelopeFieldTypes(t *testing.T) {
	dir := t.TempDir()

	// Plain top-level functions so the receiver heuristic in
	// traceVarTypeFromBody isn't on the critical path here — we want
	// to exercise *envelope iteration + per-field resolution*, not
	// the existing field-name/receiver-type matching policy. (Method
	// receivers are exercised by the broader return-type tests; this
	// test's contract is "envelope rows that traceVarTypeFromBody
	// can resolve do get patched in place".)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "service.go"), []byte(`package main

type Repo struct{ Name string }
type Workspace struct{ ID string }

func loadWorkspace(id string) (*Workspace, error) { return nil, nil }
func loadRepos(id string)     ([]Repo, error)     { return nil, nil }
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "handler.go"), []byte(`package main

import "net/http"

type Handler struct{}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{id}/repos", h.handleWorkspaceRoster)
}

func (h *Handler) handleWorkspaceRoster(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ws, _ := loadWorkspace(id)
	repos, _ := loadRepos(id)
	WriteJSON(w, http.StatusOK, map[string]any{"workspace": ws, "repos": repos})
}

func WriteJSON(w http.ResponseWriter, code int, body any) {}
`), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default()
	idx := New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	cr := idx.ContractRegistry()
	require.NotNil(t, cr)

	var found contracts.Contract
	var ok bool
	for _, c := range cr.All() {
		if c.Type == contracts.ContractHTTP && strings.Contains(c.ID, "/v1/workspaces") && strings.Contains(c.ID, "/repos") {
			found, ok = c, true
			break
		}
	}
	require.True(t, ok, "expected HTTP contract for /v1/workspaces/{p1}/repos")

	envRaw, ok := found.Meta["response_envelope"].([]map[string]any)
	require.True(t, ok, "response_envelope missing or wrong shape: %#v", found.Meta["response_envelope"])
	require.Len(t, envRaw, 2, "envelope must carry both fields")

	byName := map[string]map[string]any{}
	for _, row := range envRaw {
		if name, _ := row["name"].(string); name != "" {
			byName[name] = row
		}
	}

	wsType, _ := byName["workspace"]["type"].(string)
	if !strings.Contains(wsType, "Workspace") {
		t.Errorf("envelope[workspace].type = %q, want a Workspace ref", wsType)
	}
	reposType, _ := byName["repos"]["type"].(string)
	if !strings.Contains(reposType, "Repo") {
		t.Errorf("envelope[repos].type = %q, want a Repo ref", reposType)
	}

	// With every field typed, the schema is fully extracted — not
	// just partial. The dashboard shows the green badge.
	if got, _ := found.Meta["schema_source"].(string); got != "extracted" {
		t.Errorf("schema_source = %q, want \"extracted\" once every envelope row is typed", got)
	}

	// And the ugly `map[string]any{...}` literal must NOT be on
	// response_expr — the envelope IS the JSON shape.
	if got, _ := found.Meta["response_expr"].(string); got != "" {
		t.Errorf("response_expr should be empty when an envelope is present; got %q", got)
	}

	// Each envelope row should carry the inlined shape of its type so
	// the dashboard can render the actual JSON object structure (field
	// names, JSON tags, types) without doing a second graph lookup.
	// Workspace has one field (ID); Repo has one field (Name) — the
	// shape extractor preserves both.
	for _, key := range []string{"workspace", "repos"} {
		row, ok := byName[key]
		if !ok {
			continue
		}
		shape := row["shape"]
		if shape == nil {
			t.Errorf("envelope[%s] missing shape; want struct fields inlined", key)
			continue
		}
		// Shape is the contracts.Shape value; check it via reflection-
		// free type-assert chain. Either a *contracts.Shape (direct
		// pointer) or a struct value works — Meta is map[string]any
		// so we accept whatever the snapshot pass put there.
		var fields []contracts.ShapeField
		switch s := shape.(type) {
		case *contracts.Shape:
			fields = s.Fields
		case contracts.Shape:
			fields = s.Fields
		}
		if len(fields) == 0 {
			t.Errorf("envelope[%s].shape has no fields; got %#v", key, shape)
		}
	}
}

func TestContractsCrossFileHandlerResolvesSiblingResponseShape(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"routes.go":  "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /items\", handle) }\n",
		"handler.go": "package fixture\nimport \"net/http\"\nfunc handle(w http.ResponseWriter, r *http.Request) { response := Reply{Name:\"ok\"}; WriteJSON(w,200,response) }\nfunc WriteJSON(w http.ResponseWriter, code int, body any) {}\n",
		"types.go":   "package fixture\ntype Reply struct { Name string `json:\"name\"` }\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("fixture")
	_, err := idx.Index(dir)
	require.NoError(t, err)
	rows := idx.ContractRegistry().ByID("http::GET::/items")
	require.Len(t, rows, 1)
	require.Equal(t, "fixture/handler.go::handle", rows[0].SymbolID)
	require.Equal(t, "fixture/types.go::Reply", rows[0].Meta["response_type"])
	encoded, err := json.Marshal(rows[0].Meta["response_shape"])
	require.NoError(t, err)
	var shape contracts.Shape
	require.NoError(t, json.Unmarshal(encoded, &shape))
	require.Equal(t, "struct", shape.Kind)
	require.Len(t, shape.Fields, 1)
	require.Equal(t, "name", shape.Fields[0].Name)
	require.Equal(t, "string", shape.Fields[0].Type)
	require.Equal(t, `json:"name"`, shape.Fields[0].JSONTag)
}
