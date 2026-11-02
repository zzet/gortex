package hooks

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// stubWalkScopes makes /repo, /repo/internal and /abs/src directory scopes
// that hold indexed source, and every other directory one that does not;
// of these, /repo/internal holds source only in its subdirectories. It
// returns the scopes probed, in order.
func stubWalkScopes(t *testing.T) *[]string {
	t.Helper()
	probed := &[]string{}
	old := scopeTrackedFn
	scopeTrackedFn = func(cwd, scope string) (bool, bool) {
		if !filepath.IsAbs(scope) {
			scope = filepath.Join(cwd, scope)
		}
		scope = filepath.Clean(scope)
		*probed = append(*probed, scope)
		switch scope {
		case "/repo", "/repo/internal", "/abs/src":
			return true, true
		}
		return false, true
	}
	oldHolds := holdsSourceFn
	holdsSourceFn = func(cwd, dir string) bool {
		return filepath.Join(cwd, dir) != "/repo/internal" && dir != "/repo/internal"
	}
	t.Cleanup(func() { scopeTrackedFn, holdsSourceFn = old, oldHolds })
	return probed
}

func walkScript(body string) string {
	return "python3 - <<'EOF'\n" + body + "\nEOF"
}

func TestEnrichBash_InlinePythonWalk(t *testing.T) {
	const (
		deny   = "deny"   // the walk deny
		silent = "silent" // nothing at all
		allow  = "allow"  // no deny
		readA  = "readA"  // the shell read's deny, for a.go
	)
	walk := "import os\nfor r, d, fs in os.walk('internal'):\n    for f in fs:\n"
	read := "        s = open(os.path.join(r, f)).read()"
	for _, tc := range []struct {
		name, cmd, want, root string
	}{
		// A walk over indexed source that reads what it finds is a Grep with
		// a path: it gets that deny.
		{"the #1184 grep", walkScript("import os\nfor root, _, files in os.walk('.'):\n    for f in files:\n        if f.endswith('.go') and 'ValidateToken' in open(os.path.join(root, f)).read():\n            print(root, f)"), deny, "."},
		{"os.walk", walkScript(walk + read), deny, "internal"},
		{"source filter", walkScript(walk + "        if f.endswith('.go'):\n    " + read), deny, "internal"},
		{"source filter, then continue", walkScript(walk + "        if not f.endswith('.go'):\n            continue\n" + read), deny, "internal"},
		{"directory prune", walkScript(walk + "        if '.git' in r:\n            continue\n" + read), deny, "internal"},
		{"glob", walkScript("import glob\nfor p in glob.glob('internal/**/*.go', recursive=True):\n    print(p, open(p).read())"), deny, "internal"},
		{"Path.rglob", walkScript("from pathlib import Path\nfor p in Path('internal').rglob('*.go'):\n    s = p.read_text()"), deny, "internal"},
		{"listdir", walkScript("import os\nfor f in os.listdir('/abs/src'):\n    s = open('/abs/src/%s' % f).read()"), deny, "/abs/src"},
		{"followed cd", "cd /abs && python3 -c \"import os\nfor r, d, fs in os.walk('src'):\n    for f in fs:\n" + read + "\"", deny, "src"},
		{"nested bash -c", "bash -c \"" + walkScript(walk+read) + "\"", deny, "internal"},

		// Anything the hook cannot prove reads indexed source passes.
		{"docs filter", walkScript(walk + "        if f.endswith('.md'):\n    " + read), silent, ""},
		{"docs glob", walkScript("import glob\nfor p in glob.glob('internal/**/README*', recursive=True):\n    s = open(p).read()"), silent, ""},
		{"non-source tree", walkScript("import os\nfor r, d, fs in os.walk('docs'):\n    for f in fs:\n" + read), silent, ""},
		{"names only", walkScript(walk + "        print(os.path.join(r, f))"), silent, ""},
		{"written back", walkScript(walk + "        p = os.path.join(r, f)\n        open(p, 'w').write(open(p).read())"), allow, ""},
		{"use the hook does not follow", walkScript(walk + "        if wanted(f):\n    " + read), silent, ""},
		{"filter beside the read", walkScript(walk + "        if f.endswith('.go'):\n            print('skip', f)\n        elif verbose:\n    " + read), silent, ""},
		{"sidecar file", walkScript(walk + "        s = open(f'{r}/{f}.golden').read()"), silent, ""},
		{"directory restriction", walkScript("import os\nfor r, d, fs in os.walk('.'):\n    if not r.startswith('./docs'):\n        continue\n    for f in fs:\n" + read), silent, ""},
		{"listing of a directory with source only below", walkScript("import os\nfor f in os.listdir('internal'):\n    s = open('internal/%s' % f).read()"), silent, ""},
		{"syntax check", walkScript(walk + "        _ = ast.parse(open(os.path.join(r, f)).read())"), silent, ""},
		{"cd the hook cannot follow", "cd $V && python3 -c \"" + walk + read + "\"", silent, ""},

		// A deny main already gives keeps its reason.
		{"walk before a cat", "bash -c \"" + walkScript(walk+read) + "\"; cat a.go", readA, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectTelemetry(t)
			stubIndexedPaths(t, "a.go")
			stubWalkScopes(t)
			r := enrichBash(map[string]any{"command": tc.cmd}, "/repo")
			switch tc.want {
			case deny:
				want := "Bash Python script walks indexed source under " + tc.root + " "
				if !r.deny || !strings.Contains(r.reason, want) || !strings.Contains(r.reason, `search(operation:"text"`) {
					t.Errorf("want the walk deny naming %q, got %+v", tc.root, r)
				}
			case readA:
				if !r.deny || !strings.Contains(r.reason, " a.go` reads indexed source") {
					t.Errorf("want the shell read deny, got %+v", r)
				}
			case silent:
				if r.deny || r.context != "" {
					t.Errorf("want silence, got %+v", r)
				}
			case allow:
				if r.deny {
					t.Errorf("want no deny, got %q", r.reason)
				}
			}
		})
	}
}

// One command probes at most two walk roots, nested commands included.
func TestEnrichBash_InlinePythonWalkProbes(t *testing.T) {
	redirectTelemetry(t)
	stubIndexedPaths(t)
	probed := stubWalkScopes(t)
	walk := func(i int, root string) string {
		return fmt.Sprintf("for r%d, d%d, fs%d in os.walk('%s'):\n    for f%d in fs%d:\n        s = open(os.path.join(r%d, f%d)).read()\n", i, i, i, root, i, i, i, i)
	}
	script := walkScript("import os\n" + walk(1, "/tmp/a") + walk(2, "/tmp/b") + walk(3, "internal"))
	cmd := strings.Repeat(`bash -c "`+script+`"; `, 3) + script
	if r := enrichBash(map[string]any{"command": cmd}, "/repo"); r.deny || len(*probed) != 2 {
		t.Errorf("got %+v after probes %v, want two probes and no deny", r, *probed)
	}
}
