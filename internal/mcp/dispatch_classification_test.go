package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// Some handlers are registered only when an optional service is installed.
// Inspect their real declarations too, so adding an optional tool cannot
// silently evade the runtime full/deferred catalogue census below.
func TestDispatchClassCoversOptionalToolDeclarations(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if file.Name.Name == "mcp" {
			files[entry.Name()] = file
		}
	}
	constants := map[string]string{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			group, ok := declaration.(*ast.GenDecl)
			if !ok || group.Tok != token.CONST {
				continue
			}
			for _, item := range group.Specs {
				value := item.(*ast.ValueSpec)
				for index, expression := range value.Values {
					literal, ok := expression.(*ast.BasicLit)
					if ok && literal.Kind == token.STRING && index < len(value.Names) {
						constants[value.Names[index].Name], _ = strconv.Unquote(literal.Value)
					}
				}
			}
		}
	}
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "NewTool" {
					return true
				}
				name := ""
				switch argument := call.Args[0].(type) {
				case *ast.BasicLit:
					name, _ = strconv.Unquote(argument.Value)
				case *ast.Ident:
					name = constants[argument.Name]
					if argument.Name == "name" && function.Name.Name == "facadeToolDefinitionWithOperations" {
						for _, facade := range facadeToolNames() {
							if _, known := daemon.ToolDispatchClass(facade); !known {
								t.Errorf("facade %q has no class", facade)
							}
						}
						return true
					}
				}
				if _, known := daemon.ToolDispatchClass(name); !known {
					t.Errorf("tool declaration at %s has no class (resolved name %q)", fset.Position(call.Pos()), name)
				}
				return true
			})
		}
	}
}

func TestDispatchClassCoversRegisteredAndFacadeTools(t *testing.T) {
	for _, config := range []ToolPolicyConfig{{Preset: "full"}, {Preset: "core", Mode: "defer"}} {
		srv := setupPresetServer(t, config)
		for _, name := range srv.registeredToolNames() {
			if _, known := daemon.ToolDispatchClass(name); !known {
				t.Errorf("registered tool %q has no explicit dispatch class", name)
			}
		}
		for _, spec := range facadeOperationSpecs() {
			if _, known := daemon.ToolDispatchClass(spec.Legacy); !known {
				t.Errorf("facade %s.%s implementation %q has no dispatch class", spec.Facade, spec.Operation, spec.Legacy)
			}
		}
	}
}

func TestDispatchClassUsesEffectiveFacadeOperation(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "full"})
	cases := []struct {
		name string
		args map[string]any
		want daemon.MCPDispatchClass
	}{
		{"edit_file", nil, daemon.MCPDispatchMutating},
		{"edit", map[string]any{"operation": "file"}, daemon.MCPDispatchMutating},
		{"refactor", map[string]any{"operation": "rename"}, daemon.MCPDispatchMutating},
		{"read", map[string]any{"operation": "file", "target": map[string]any{"file": "just-edited.go"}}, daemon.MCPDispatchFast},
		{"explore", map[string]any{"operation": "outline"}, daemon.MCPDispatchSlow},
		{"analyze", map[string]any{"kind": "dead_code"}, daemon.MCPDispatchSlow},
		{"analyze", map[string]any{"kind": "processes"}, daemon.MCPDispatchSlow},
		{"analyze", map[string]any{"kind": "help"}, daemon.MCPDispatchSlow},
		{"workspace_admin", map[string]any{"operation": "enrich_churn"}, daemon.MCPDispatchMutating},
		{"change_contract", map[string]any{"ack": true}, daemon.MCPDispatchMutating},
		{"change", map[string]any{"operation": "contract", "options": map[string]any{"ack": true}}, daemon.MCPDispatchFast},
		{"find_co_changing_symbols", nil, daemon.MCPDispatchMutating},
		{"analyze", map[string]any{"kind": "co_change", "options": map[string]any{"refresh": true}}, daemon.MCPDispatchSlow},
		{"recall", map[string]any{"operation": "surface"}, daemon.MCPDispatchFast},
		{"remember", map[string]any{"operation": "risk_ack"}, daemon.MCPDispatchMutating},
		{"overlay", map[string]any{"operation": "merge", "arguments": map[string]any{"to_disk": true}}, daemon.MCPDispatchFast},
		{"read", map[string]any{"operation": "symbol", "target": map[string]any{"symbol": "demo::f"}}, daemon.MCPDispatchFast},
		{"session", map[string]any{"operation": "subscribe", "channel": "diagnostics"}, daemon.MCPDispatchFast},
		{"edit", map[string]any{"operation": "not_registered"}, daemon.MCPDispatchFast},
		{"not_registered", nil, daemon.MCPDispatchFast},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := srv.DispatchClass(test.name, test.args); got != test.want {
				t.Errorf("class=%s want=%s", got, test.want)
			}
		})
	}
}
