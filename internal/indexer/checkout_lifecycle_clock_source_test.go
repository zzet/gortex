package indexer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every method of CheckoutLifecycle reads the clock through clock(), which
// falls back to time.Now for a lifecycle built without one (a struct
// literal); a direct call of the now field panics on such a lifecycle.
// TestIdleReleaseClockOnALifecycleWithoutOne calls the reads a bare or
// minimal lifecycle can reach; this pins the rest, in every file of the
// package, by their source.
func TestCheckoutLifecycleReadsTheClockOnlyThroughClock(t *testing.T) {
	if direct := lifecycleDirectClockCalls(t, "."); len(direct) > 0 {
		t.Fatalf("CheckoutLifecycle methods call the now field directly (use clock()):\n%s", strings.Join(direct, "\n"))
	}
	// The scan finds what it is for: a copy of one real file with its read
	// put back to a direct call is caught, and clock() itself is not.
	dir := t.TempDir()
	src, err := os.ReadFile("checkout_deferred_retirement.go")
	if err != nil {
		t.Fatal(err)
	}
	reverted := strings.Replace(string(src), "l.clock().Add(-abandonedBuildingGrace)", "l.now().Add(-abandonedBuildingGrace)", 1)
	if reverted == string(src) {
		t.Fatal("the probe's clock read is gone from checkout_deferred_retirement.go; point the probe at another")
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(reverted), 0o644); err != nil {
		t.Fatal(err)
	}
	clock := "package indexer\n\nfunc (l *CheckoutLifecycle) clock() time.Time {\n\tif l.now != nil {\n\t\treturn l.now()\n\t}\n\treturn time.Now()\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "clock.go"), []byte(clock), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleDirectClockCalls(t, dir); len(got) != 1 || !strings.Contains(got[0], "discoverDeferredRetirementsWith") {
		t.Fatalf("the scan of a reverted read found %v, want that one call", got)
	}
}

// lifecycleDirectClockCalls lists the direct calls of the now field in the
// CheckoutLifecycle methods of dir's non-test Go files, clock() excepted.
func lifecycleDirectClockCalls(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var direct []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil || fn.Name.Name == "clock" {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "CheckoutLifecycle" || len(fn.Recv.List[0].Names) != 1 {
				continue
			}
			recv := fn.Recv.List[0].Names[0].Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "now" {
					return true
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == recv {
					direct = append(direct, fset.Position(call.Pos()).String()+" in "+fn.Name.Name)
				}
				return true
			})
		}
	}
	return direct
}
