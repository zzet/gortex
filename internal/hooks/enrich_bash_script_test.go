package hooks

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubIndexedPaths makes exactly the named paths look like indexed source.
func stubIndexedPaths(t *testing.T, indexed ...string) {
	t.Helper()
	stubFileIndexScopeBy(t, func(_ string, path string) fileIndexStatus {
		for _, p := range indexed {
			if path == p {
				return indexedStatus(12)
			}
		}
		return fileIndexStatus{Tracked: false, ProbeOK: true}
	})
}

func TestEnrichBash_InlinePython(t *testing.T) {
	const (
		deny    = "deny"    // the Read deny
		edit    = "edit"    // the shell-write advisory
		maybe   = "maybe"   // the shell-write advisory, for a write it cannot name
		silent  = "silent"  // nothing at all
		allow   = "allow"   // no deny (another door may still advise)
		catDeny = "catDeny" // the shell read's own deny
		readA   = "readA"   // the shell read's deny, for a.go
		sedA    = "sedA"    // the shell-write advisory, for sed -i a.go
		scriptW = "scriptW" // the shell-write advisory, for a script write to a.go

		pyRead = `python3 -c "print(open('a.go').read())"`
	)
	for _, tc := range []struct {
		name, cmd, want string
	}{
		// Reads of indexed source get the Read deny.
		{"open", `python3 -c "print(open('a.go').read())"`, deny},
		{"pathlib", `python -c "from pathlib import Path; print(Path('a.go').read_text())"`, deny},
		{"through a variable", `python3 -c "p = 'a.go'; s = open(p).read(); print(len(s))"`, deny},
		{"joined literal", `python3 -c "import os; print(open(os.path.join('src', 'b.go')).read())"`, deny},
		{"heredoc", "python3 - <<'EOF'\nimport ast\nt = ast.parse(open('a.go').read())\nprint(ast.dump(t))\nEOF", deny},
		{"launcher", `uv run --with rich python3 -c "print(open('a.go').read())"`, deny},
		{"versioned binary", `python3.12 -uc "print(open('a.go').read())"`, deny},
		{"-W value attached", `python3 -Werror::DeprecationWarning -c "print(open('a.go').read())"`, deny},
		{"read, then write elsewhere", `python3 -c "s=open('a.go').read(); open('/tmp/out.txt','w').write(s)"`, deny},
		{"cd after the script", `python3 -c "print(open('a.go').read())" && cd /tmp`, deny},

		// Chained after a shell read of a non-indexed path, the trailing
		// head makes the whole command non-passthrough, so the Python door
		// must run for every command, not only when the shell leaves it
		// passthrough.
		{"python read after a head", `python3 -c "print(open('a.go').read())" && head /tmp/other.go`, deny},
		{"python read before a head", `head /tmp/other.go && python3 -c "print(open('a.go').read())"`, deny},
		{"python read after a tail", `python3 -c "print(open('a.go').read())" && tail /tmp/other.go`, deny},

		// A file read and written back is an edit.
		{"read-modify-write", "python3 - <<'EOF'\nfrom pathlib import Path\np = Path('a.go'); s = p.read_text()\np.write_text(s.replace('x', 'y'))\nEOF", edit},
		{"r+", `python3 -c "f=open('a.go','r+'); s=f.read(); f.seek(0); f.write(s)"`, edit},
		{"expression write target", "python3 - <<'EOF'\nimport os\nf = 'a.go'\ns = open(f).read()\nopen(os.path.abspath(f), 'w').write(s)\nEOF", edit},
		{"unnamed write target", `python3 -c "s=open('a.go').read(); open(sys.argv[1], 'w').write(s)"`, maybe},
		{"edit wins over reads", `python3 -c "print(open('b.go').read())" && python3 -c "p='a.go'; s=open(p).read(); open(p,'w').write(s)"`, edit},

		// A Python edit after a shell read of a non-indexed path is still an edit.
		{"python edit after a head", "head /tmp/other.go && python3 -c \"p='a.go'; s=open(p).read(); open(p,'w').write(s)\"", edit},

		// The edit advisory never softens a deny the shell read earns itself.
		{"python edit after a source cat", "cat b.go && python3 -c \"p='a.go'; s=open(p).read(); open(p,'w').write(s)\"", catDeny},
		{"python edit before a source cat", "python3 -c \"p='a.go'; s=open(p).read(); open(p,'w').write(s)\" && cat b.go", catDeny},

		// Everything else passes.
		{"pure write", `python3 -c "open('a.go', \"w\").write('package a')"`, allow},
		{"pure write, plain quotes", `python3 -c "open('a.go', 'w').write('package a')"`, scriptW},
		{"syntax check", `python -c 'import ast; ast.parse(open("a.go").read()); print("syntax ok")'`, silent},
		{"data file", `python3 -c "import json; print(json.load(open('package.json')))"`, silent},
		{"unindexed source", `python3 -c "print(open('c.go').read())"`, silent},
		{"outside the repo", `python3 -c "print(open('/home/u/.gemini/brain/x/output.txt').read())"`, silent},
		{"path inside a string", `python3 -c "print(\"open('a.go') is how\")"`, silent},
		{"rebound before the read", `python3 -c "import sys\np = 'a.go'\np = sys.argv[1]\nprint(open(p).read())"`, silent},
		{"attribute is not a name", "python3 -c \"import os\npath = 'a.go'\nprint(open(os.path.join(os.getcwd(), 'Makefile')).read())\"", silent},
		{"global rebinding", "python3 -c \"p = 'a.go'\ndef setp():\n    global p\n    p = 'notes.txt'\nsetp()\nprint(open(p).read())\"", silent},
		{"late-bound closure", "python3 -c \"PATH = 'a.go'\ndef main():\n    print(open(PATH).read())\nPATH = '/tmp/notes.txt'\nmain()\"", silent},
		{"closure over a name bound once", "python3 -c \"PATH = 'a.go'\ndef main():\n    print(open(PATH).read())\nmain()\"", deny},
		{"class body is not a method's scope", "python3 -c \"class C:\n    p = 'a.go'\n    def show(self): print(open(p).read())\np = 'notes.txt'\nC().show()\"", silent},
		{"walrus in a comprehension", "python3 -c \"p = 'a.go'\nany((p := 'notes.txt') for _ in [1])\nprint(open(p).read())\"", silent},
		{"mode that is not a literal", "python3 -c \"m = 'w'\nopen('a.go', m).write('x')\"", silent},
		{"a local open", "python3 - <<'PY'\ndef open(p, *a):\n    return __builtins__.open('/tmp/x')\nopen('a.go')\nPY", silent},
		{"here-string then heredoc", "python3 - <<< 'print(1)' && cat <<'EOF'\nprint(open('a.go').read())\nEOF", silent},
		{"rebound later in a loop, read in a comprehension", "python3 - <<'PY'\np = 'a.go'\nfor i in range(2):\n    if i == 1:\n        x = [open(p).read() for _ in [0]]\n    p = 'x.txt'\nPY", silent},
		{"two heredocs on one line", "python3 - <<'PY' && cat <<'NOTE'\nprint('ok')\nPY\nprint(open('a.go').read())\nNOTE", silent},
		{"heredoc text echoed before a cd", "echo \"open('a.go')\"; cd /tmp && python3 - <<'X'\nopen('a.go')\nX", silent},
		{"default in a class body", "python3 - <<'PY'\np = 'a.go'\nclass C:\n    p = 'notes.txt'\n    def m(self, s=open(p).read()):\n        return s\nPY", silent},
		{"comprehension iterable in a class body", "python3 - <<'PY'\np = 'a.go'\nclass C:\n    p = 'notes.txt'\n    lines = [l for l in open(p)]\nPY", silent},
		{"default reads source", "python3 - <<'PY'\ndef m(s=open('a.go').read()):\n    return s\nPY", deny},
		{"global in a class body", "python3 - <<'PY'\np = 'a.go'\nclass C:\n    global p\n    p = 'notes.txt'\nprint(open(p).read())\nPY", silent},
		{"absolute join part", `python3 -c "import os; print(open(os.path.join('src', '/b.go')).read())"`, silent},
		{"aliased chdir", `python3 -c "from os import chdir as cd; cd('/tmp'); print(open('a.go').read())"`, silent},
		{"annotated parameter with a default", "python3 - <<'PY'\ntarget = 'a.go'\ndef show(target: str = 'notes.txt', /, *a, k: int = 1, **kw):\n    print(open(target).read())\nshow()\nPY", silent},
		{"annotated parameter in a patch", "python3 - <<'PY'\ntarget = 'a.go'\ndef patch(target: str = 'notes.txt'):\n    s = open(target).read()\n    open(target, 'w').write(s)\nPY", silent},
		{"default naming a module path", "python3 - <<'PY'\np = 'a.go'\ndef show(q=p):\n    print(open(q).read())\nPY", silent},
		{"match capture", "python3 - <<'PY'\np = 'a.go'\nmatch {'path': 'x.txt'}:\n    case {'path': str() as p}:\n        print(open(p).read())\n    case [q, *p] | {'k': 1, **p}:\n        pass\nPY", silent},
		{"walrus in a default", "python3 - <<'PY'\np = 'a.go'\ndef f(x=(p := 'x.txt')): pass\nprint(open(p).read())\nPY", silent},
		{"walrus in class bases", "python3 - <<'PY'\np = 'a.go'\nclass C(object if (p := 'x.txt') else int, metaclass=type): pass\nprint(open(p).read())\nPY", silent},
		{"walrus in a return annotation", "python3 - <<'PY'\np = 'a.go'\ndef f() -> (p := 'x.txt'): pass\nprint(open(p).read())\nPY", silent},
		{"rebound later in a loop", "python3 - <<'PY'\np = 'a.go'\nfor i in range(2):\n    if i == 1:\n        print(open(p).read())\n    p = 'x.txt'\nPY", silent},
		{"loop that does not rebind", "python3 - <<'PY'\np = 'a.go'\nfor i in range(2):\n    print(open(p).read())\nPY", deny},
		{"local read before its binding", "python3 - <<'PY'\np = 'a.go'\ndef f():\n    print(open(p).read())\n    p = 'x.txt'\nf()\nPY", silent},
		{"del", "python3 - <<'PY'\np = 'a.go'\ndel p\nprint(open(p).read())\nPY", silent},
		{"type parameter", "python3 - <<'PY'\np = 'a.go'\ndef f[p](x):\n    return open(p).read()\nPY", silent},
		{"global in a nested block", "python3 -c \"p = 'a.go'\ndef s():\n    if True:\n        global p\n        p = 'notes.txt'\ns()\nprint(open(p).read())\"", silent},
		{"tarfile.open", `python3 -c "import tarfile; tarfile.open('a.go')"`, silent},
		{"module run", `python3 -m pytest a.go`, silent},
		{"script file", `python3 report.py a.go`, silent},
		{"other interpreter", `node -e "console.log(require('fs').readFileSync('a.go','utf8'))"`, silent},
		{"does not parse", `python3 -c "print(open('a.go').read()"`, silent},

		// A relative path after a directory change the hook cannot follow.
		{"cd to a variable", "cd $V && python3 -c \"print(open('a.go').read())\"", silent},
		{"subshell cd", `(cd /tmp/x && python3 -c "print(open('a.go').read())")`, silent},
		{"chdir", `python3 -c "from os import chdir; chdir('/tmp'); print(open('a.go').read())"`, silent},
		{"env -C", `env -C /tmp python3 -c "print(open('a.go').read())"`, silent},
		{"uv run --directory", `uv run --directory=/tmp python -c "print(open('a.go').read())"`, silent},
		{"env -C attached", `env -iC/tmp python3 -c "print(open('a.go').read())"`, silent},

		// Wrappers that run the command in the same filesystem and directory
		// are looked through; those that run it against another root, and
		// `command -v`, which runs nothing, are not.
		{"exec", "exec " + pyRead, deny},
		{"nohup", "nohup " + pyRead, deny},
		{"setsid", "setsid -f " + pyRead, deny},
		{"nice -n", "nice -n 5 " + pyRead, deny},
		{"nice -N", "nice -5 " + pyRead, deny},
		{"nice --adjustment", "nice --adjustment=3 " + pyRead, deny},
		{"command", "command " + pyRead, deny},
		{"nvm exec", "nvm exec 20 " + pyRead, deny},
		{"nvm exec without a version", "nvm exec " + pyRead, deny},
		{"direnv exec", "direnv exec . " + pyRead, deny},
		{"wrapped launcher", "timeout 5 nohup " + pyRead, deny},
		{"command -v", "command -v " + pyRead, silent},
		// The shell read and write shapes do not look through them: a wrapped
		// first statement must not stop the classification before a later one.
		// What the hook newly sees (os.open reads, wrapped scripts) has its
		// own probe budget: it never spends the one a later read needs.
		{"os.open reads before a read", `python3 -c "import os; os.open('x0.py', os.O_RDONLY); os.open('x1.py', os.O_RDONLY); os.open('x2.py', os.O_RDONLY); print(open('a.go').read())"`, deny},
		{"os.open creates before a read", `python3 -c "import os; os.open('/tmp/x0.py', os.O_CREAT); os.open('/tmp/x1.py', os.O_CREAT); os.open('/tmp/x2.py', os.O_CREAT); print(open('a.go').read())"`, deny},
		{"os.open reads, then a read", `python3 -c "import os; os.open('x0.py', os.O_RDONLY); os.open('x1.py', os.O_RDONLY); os.open('x2.py', os.O_RDONLY)" && ` + pyRead, deny},
		{"wrapped reads before a read", `nohup python3 -c "print(open('x0.py').read())"; nohup python3 -c "print(open('x1.py').read())"; nohup python3 -c "print(open('x2.py').read())"; ` + pyRead, deny},
		{"wrapped script reading three files before a read", `nice -n 5 python3 -c "print(open('x0.py').read(), open('x1.py').read(), open('x2.py').read())"; ` + pyRead, deny},
		{"wrapped rewrites before a rewrite", `exec python3 -c "p='x0.py'; s=open(p).read(); open(p,'w').write(s)"; exec python3 -c "p='x1.py'; s=open(p).read(); open(p,'w').write(s)"; exec python3 -c "p='x2.py'; s=open(p).read(); open(p,'w').write(s)"; python3 -c "p='a.go'; s=open(p).read(); open(p,'w').write(s)"`, edit},
		{"wrapped command, then a cat", `nohup ls && cat a.go`, readA},
		{"command, then a tail", `command ls; tail a.go`, readA},
		{"wrapped cat of other source, then a cat", `nice -n 5 cat c.go && cat a.go`, readA},
		{"wrapped cat", `nohup cat a.go`, silent},
		{"chroot", "chroot /x " + pyRead, silent},

		// os.open reads and writes by its literal flags.
		{"os.open read", `python3 -c "import os; fd = os.open('a.go', os.O_RDONLY); print(os.read(fd, 99))"`, deny},
		{"aliased os.open", `python3 -c "from os import open as o, read, O_RDONLY; print(read(o('a.go', O_RDONLY), 99))"`, silent},
		{"os.open bare flag", `python3 -c "import os; from os import O_RDONLY, O_CLOEXEC; print(os.read(os.open('a.go', O_RDONLY | O_CLOEXEC), 99))"`, deny},
		// An os.open write is a write target only: never a read, never what
		// turns a read into an edit, so it hides no Read deny.
		{"os.open write and a read of the same file", `python3 -c "import os; print(open('a.go').read()); os.open('a.go', os.O_WRONLY)"`, deny},
		{"os.open read-write and a read of another file", `python3 -c "import os; os.open('a.go', os.O_RDWR); print(open('b.go').read())"`, deny},
		{"Python read, then an os.open read-write", `python3 -c "print(open('b.go').read())" && python3 -c "import os; os.open('a.go', os.O_RDWR)"`, deny},
		{"os.open O_PATH", `python3 -c "import os; os.open('a.go', os.O_RDONLY | os.O_PATH)"`, silent},
		{"os.open O_DIRECTORY", `python3 -c "import os; os.open('a.go', os.O_DIRECTORY)"`, silent},
		{"os.open read-modify-write", "python3 - <<'PY'\nimport os\nfd = os.open('a.go', os.O_RDWR)\ns = os.read(fd, 1 << 20)\nos.lseek(fd, 0, 0)\nos.write(fd, s)\nPY", scriptW},
		{"os.open write", `python3 -c "import os; os.write(os.open('a.go', os.O_WRONLY | os.O_TRUNC), b'package a')"`, scriptW},
		{"os.open variable flags", `python3 -c "import os, sys; f = int(sys.argv[1]); print(os.read(os.open('a.go', f), 99))"`, silent},
		{"os.open flag not from os", `python3 -c "import os; print(os.read(os.open('a.go', O_RDONLY), 99))"`, silent},
		{"os.open create", `python3 -c "import os; print(os.read(os.open('a.go', os.O_RDONLY | os.O_CREAT), 99))"`, deny},
		{"os.open dir_fd", `python3 -c "import os; d = os.open('src', os.O_RDONLY); print(os.read(os.open('a.go', os.O_RDONLY, dir_fd=d), 99))"`, silent},
		{"os aliased onto another module", `python3 -c "import mymod as os; print(os.read(os.open('a.go', os.O_RDONLY), 99))"`, silent},

		// An os.open write never softens a deny the rest of the command earns.
		{"os.open write, then a cat", `python3 -c "import os; os.open('a.go', os.O_WRONLY)" && cat b.go`, catDeny},
		{"os.open write after a cat", `cat b.go && python3 -c "import os; os.open('a.go', os.O_WRONLY)"`, catDeny},
		{"os.open write, then a head", `python3 -c "import os; os.open('a.go', os.O_WRONLY)" && head b.go`, deny},
		{"os.open write and a read", `python3 -c "import os; os.open('a.go', os.O_WRONLY); print(open('b.go').read())"`, deny},
		{"os.open write, then a Python read", `python3 -c "import os; os.open('a.go', os.O_WRONLY)" && python3 -c "print(open('b.go').read())"`, deny},
		{"open write, then a cat", `python3 -c "open('a.go', 'w').write('x')" && cat b.go`, scriptW},
		{"os.open write, unindexed", `python3 -c "import os; os.write(os.open('c.go', os.O_WRONLY), b'x')"`, silent},

		// A command run as text gets the answer it would get typed: the
		// -c string of a shell, and a literal command line Python runs.
		{"bash -c", `bash -c "cat a.go"`, readA},
		{"bash -lc", `bash -lc 'head -5 a.go'`, readA},
		{"sh with options", `sh -o pipefail -ec 'cat a.go'`, readA},
		{"wrapped shell", `nohup bash -c 'cat a.go'`, readA},
		{"python in a shell", `sh -c 'python3 -c "print(open(\"a.go\").read())"'`, deny},
		{"shell write in a shell", `bash -c "sed -i s/x/y/ a.go"`, sedA},
		{"nested deny beats an outer advisory", `sed -i s/x/y/ a.go && bash -c 'cat b.go'`, catDeny},
		{"os.popen", `python3 -c "import os; print(os.popen('cat a.go').read())"`, readA},
		{"os.system", `python3 -c "import os; os.system('cat a.go')"`, readA},
		{"subprocess list", `python3 -c "import subprocess; subprocess.run(['cat', 'a.go'])"`, readA},
		{"check_output tuple", `python3 -c "import subprocess; print(subprocess.check_output(('tail', '-n', '5', 'a.go')))"`, readA},
		{"getoutput", `python3 -c "import subprocess; print(subprocess.getoutput('head a.go'))"`, readA},
		{"subprocess shell write", `python3 -c "import subprocess; subprocess.run('sed -i s/x/y/ a.go', shell=True)"`, sedA},
		{"subprocess python", `python3 -c "import subprocess; subprocess.run(['python3', '-c', 'print(open(\'a.go\').read())'])"`, silent},
		{"subprocess python, quoted", "python3 - <<'PY'\nimport subprocess\nsubprocess.run(['python3', '-c', \"print(open('a.go').read())\"])\nPY", deny},

		{"module imported with a submodule", `python3 -c "import os, os.path; os.system('cat a.go')"`, readA},
		{"subprocess string with shell=True", `python3 -c "import subprocess; print(subprocess.check_output('cat a.go', shell=True))"`, readA},
		{"subprocess string without a shell", `python3 -c "import subprocess; subprocess.run('cat a.go')"`, silent},
		{"os.system list", `python3 -c "import os; os.system(['cat', 'a.go'])"`, silent},
		{"os aliased onto another module, system", `python3 -c "import mylib as os; os.system('cat a.go')"`, silent},
		{"subprocess rebound", "python3 - <<'PY'\nimport subprocess\nsubprocess = None\nsubprocess.run(['cat', 'a.go'])\nPY", silent},
		{"local class os", "python3 - <<'PY'\nclass os:\n    def system(c): pass\nos.system('cat a.go')\nPY", silent},
		{"os never imported", `python3 -c "os.system('cat a.go')"`, silent},
		{"nested script file", `bash run.sh a.go`, silent},
		{"nested unindexed", `bash -c "cat c.go" && python3 -c "import os; os.system('cat c.go')"`, silent},
		{"two levels", `bash -c "bash -c 'cat a.go'"`, silent},
		{"two levels through python", `bash -c 'python3 -c "import os; os.system(\"cat a.go\")"'`, silent},
		{"shell list from python", `python3 -c "import subprocess; subprocess.run(['bash', '-c', 'cat a.go'])"`, silent},
		{"subprocess cwd", `python3 -c "import subprocess; subprocess.run(['cat', 'a.go'], cwd='/tmp')"`, silent},
		{"subprocess executable", `python3 -c "import subprocess; subprocess.run('cat a.go', shell=True, executable='/bin/zsh')"`, silent},
		{"subprocess *args", `python3 -c "import subprocess, sys; subprocess.run(['cat', 'a.go'], *sys.argv[1:])"`, silent},
		{"subprocess **kwargs", `python3 -c "import subprocess; kw = {'cwd': '/tmp'}; subprocess.run(['cat', 'a.go'], **kw)"`, silent},
		{"subprocess variable", `python3 -c "import subprocess; c = ['cat', 'a.go']; subprocess.run(c)"`, silent},
		{"os.system f-string", `python3 -c "import os; f = 'a.go'; os.system(f'cat {f}')"`, silent},
		{"list handed to the shell", `python3 -c "import subprocess; subprocess.run(['cat a.go'], shell=True)"`, silent},
		{"nested after cd to a variable", `cd $V && bash -c "cat a.go"`, silent},
		{"python nested after cd to a variable", `cd $V && python3 -c "import os; os.system('cat a.go')"`, silent},
		{"nested after chdir", `python3 -c "import os; os.chdir('/tmp'); os.system('cat a.go')"`, silent},
		{"nested under env -C", `env -C /tmp bash -c "cat a.go"`, silent},
		{"within the nested bound", strings.Repeat(`bash -c "cat c.go"; `, 7) + `bash -c "cat a.go"`, readA},
		{"past the nested bound", strings.Repeat(`bash -c "cat c.go"; `, 8) + `bash -c "cat a.go"`, silent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectTelemetry(t)
			stubIndexedPaths(t, "a.go", "b.go", filepath.Join("src", "b.go"), "src/b.go")
			t.Setenv("GORTEX_HOOK_BLOCK_EDIT", "")
			r := enrichBash(map[string]any{"command": tc.cmd}, "/repo")
			switch tc.want {
			case deny:
				if !r.deny || !strings.Contains(r.reason, "reads indexed source") {
					t.Errorf("want the Read deny, got %+v", r)
				}
			case edit:
				if r.deny || !strings.Contains(r.context, "Python script rewrites") {
					t.Errorf("want the edit advisory, got %+v", r)
				}
			case catDeny:
				if !r.deny || !strings.Contains(r.reason, "Bash `cat b.go` reads indexed source") {
					t.Errorf("want the cat deny, got %+v", r)
				}
			case readA:
				if !r.deny || !strings.Contains(r.reason, " a.go` reads indexed source") {
					t.Errorf("want the shell read deny, got %+v", r)
				}
			case sedA, scriptW:
				shape := map[string]string{sedA: "sed -i", scriptW: "script write"}[tc.want]
				if r.deny || !strings.Contains(r.context, "Bash `"+shape+"` writes a.go") {
					t.Errorf("want the %s advisory, got %+v", shape, r)
				}
			case maybe:
				if r.deny || !strings.Contains(r.context, "Python script may rewrite") {
					t.Errorf("want the hedged edit advisory, got %+v", r)
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

// With edit blocking on, the edit advisory becomes a deny, as every other
// shell write does.
func TestEnrichBash_InlinePythonEditBlocking(t *testing.T) {
	redirectTelemetry(t)
	stubIndexedPaths(t, "a.go")
	t.Setenv("GORTEX_HOOK_BLOCK_EDIT", "1")
	r := enrichBash(map[string]any{"command": `python3 -c "p='a.go'; s=open(p).read(); open(p,'w').write(s)"`}, "/repo")
	if !r.deny || !strings.Contains(r.reason, "rewrites a.go") {
		t.Errorf("want the edit deny, got %+v", r)
	}
}

// A write-only os.open gets the shell-write deny under edit blocking, as
// open(p, 'w') does.
func TestEnrichBash_InlinePythonOSOpenEditBlocking(t *testing.T) {
	redirectTelemetry(t)
	stubIndexedPaths(t, "a.go")
	t.Setenv("GORTEX_HOOK_BLOCK_EDIT", "1")
	for _, cmd := range []string{
		`python3 -c "open('a.go', 'w').write('x')"`,
		`python3 -c "import os; os.open('a.go', os.O_WRONLY)"`,
	} {
		r := enrichBash(map[string]any{"command": cmd}, "/repo")
		if !r.deny || !strings.Contains(r.reason, "Bash `script write` writes a.go") {
			t.Errorf("%s: want the shell-write deny, got %+v", cmd, r)
		}
	}
}

// What only the extra analysis looks for (os.open, command lines) costs
// the read analysis nothing: a script it covers just under its step budget
// keeps the Read deny however many os.open calls it carries.
func TestEnrichBash_InlinePythonExtraAnalysisBudget(t *testing.T) {
	redirectTelemetry(t)
	stubIndexedPaths(t, "a.go")
	t.Setenv("GORTEX_HOOK_BLOCK_EDIT", "")
	lines := []string{"import os", "print(open('a.go').read())"}
	for i := 0; i < 90; i++ {
		lines = append(lines, "os.open("+strings.Repeat("'a'+", 199)+"'a', os.O_RDONLY)")
	}
	code := strings.Join(lines, "\n")
	if _, ok := analyzePythonScript(code, time.Now().Add(time.Minute), true); ok {
		t.Fatal("the extra analysis of this script should exceed its own step budget")
	}
	facts, ok := analyzePythonScript(code, time.Now().Add(time.Minute), false)
	if !ok || len(facts.reads) != 1 || facts.reads[0] != "a.go" {
		t.Fatalf("the read analysis must cover the script, got %v %+v", ok, facts)
	}
	// The whole policy, under a deadline that a slow (-race) build cannot
	// miss.
	cmd := "python3 - <<'EOF'\n" + code + "\nEOF"
	r := enrichBashCommand(cmd, "/repo", queryFileIndexScope, 0, time.Now().Add(time.Minute))
	if !r.deny || !strings.Contains(r.reason, "reads indexed source") {
		t.Errorf("want the Read deny, got %+v", r)
	}
}

// A redirected Python patch script is counted with the other shell writes.
func TestEnrichBash_InlinePythonEditTelemetry(t *testing.T) {
	log := redirectTelemetry(t)
	stubIndexedPaths(t, "a.go")
	t.Setenv("GORTEX_HOOK_BLOCK_EDIT", "")
	enrichBash(map[string]any{"command": `python3 -c "p='a.go'; s=open(p).read(); open(p,'w').write(s)"`}, "/repo")
	records := readDecisions(t, log)
	if len(records) != 1 || records[0].Decision != DecisionRedirectedWrite || records[0].Tool != "Bash" {
		t.Fatalf("decision log = %+v, want one Bash %q record", records, DecisionRedirectedWrite)
	}
}

// A checkout whose Gortex writes are refused keeps native reads and edits,
// as the Read and Edit doors do.
func TestEnrichBash_InlinePythonReadOnlyCheckout(t *testing.T) {
	redirectTelemetry(t)
	readOnly := func(string, string) fileIndexStatus {
		st := indexedStatus(12)
		st.ReadOnly = true
		return st
	}
	for _, cmd := range []string{
		`python3 -c "print(open('a.go').read())"`,
		`python3 -c "p='a.go'; s=open(p).read(); open(p,'w').write(s)"`,
	} {
		r := enrichBashWithProbe(map[string]any{"command": cmd}, "/repo", readOnly)
		if r.deny || !r.readOnlyAllowance {
			t.Errorf("%s: want the read-only allowance, got %+v", cmd, r)
		}
	}
}

func TestPythonInline(t *testing.T) {
	for _, tc := range []struct {
		cmd, code     string
		stdin, inline bool
	}{
		{`python3 -c "print(1)"`, "print(1)", false, true},
		{`python3 -X dev -uc 'print(1)'`, "print(1)", false, true},
		{`python3 -c "open('a', \"w\")"`, `open('a', "w")`, false, true},
		{`python3 -`, "", true, true},
		{`python3 -m pytest -q`, "", false, false},
		{`python3 script.py -c x`, "", false, false},
	} {
		words, _ := pythonCommand(shellWords(tc.cmd))
		code, stdin, ok := pythonInline(words)
		if code != tc.code || stdin != tc.stdin || ok != tc.inline {
			t.Errorf("%s: got (%q, %v, %v)", tc.cmd, code, stdin, ok)
		}
	}
	for _, cmd := range []string{`env | sort`, `timeout 5 make test`, `uv pip install rich`, `node -e "x"`} {
		if words, _ := pythonCommand(shellWords(cmd)); words != nil {
			t.Errorf("%s: seen as python", cmd)
		}
	}
}

// Each repeat of an identical statement gets its own position, so a cd
// between repeats is ordered correctly.
func TestPythonScriptsAt_PositionsAdvance(t *testing.T) {
	stmt := `python3 -c "print(open('a.go').read())"`
	cmd := strings.Join([]string{stmt, stmt, "cd /tmp", stmt}, " && ")
	scripts := pythonScriptsAt(cmd)
	cds := cdPositions(cmd, 0)
	if len(scripts) != 3 || len(cds) != 1 || scripts[1].pos <= scripts[0].pos ||
		cds[0] <= scripts[1].pos || scripts[2].pos <= cds[0] {
		t.Errorf("scripts %+v and cd %v are not ordered", scripts, cds)
	}
}

// Work is bounded: the daemon probe budget covers the whole command, and a
// pathological script finishes quickly or gets no opinion.
func TestEnrichBash_InlinePythonIsBounded(t *testing.T) {
	redirectTelemetry(t)
	probes := 0
	stubFileIndexScopeBy(t, func(string, string) fileIndexStatus {
		probes++
		return fileIndexStatus{Tracked: false, ProbeOK: true}
	})
	var many []string
	for i := 0; i < 200; i++ {
		many = append(many, fmt.Sprintf(`python3 -c "print(open('f%d.go').read())"`, i))
	}
	enrichBash(map[string]any{"command": strings.Join(many, " && ")}, "/repo")
	if probes > bashWriteProbeLimit {
		t.Errorf("200 one-liners made %d probes, want at most %d", probes, bashWriteProbeLimit)
	}
	if testing.Short() {
		return
	}
	nested := "a"
	for i := 0; i < 26; i++ {
		nested = `f"{` + nested + `}"`
	}
	deep := "x=" + strings.Repeat("(", 195) + "str(" + strings.Repeat("a,", 2000) + "a)" + strings.Repeat(")", 195) + "\n"
	var blocks strings.Builder
	for i := 0; i < 350; i++ {
		blocks.WriteString(strings.Repeat(" ", i) + "if a:\n")
	}
	blocks.WriteString(strings.Repeat(" ", 350) + "open(p)\n")
	for _, cmd := range []string{
		"python3 - <<'PY'\nx=" + nested + "\nPY",
		"python3 - <<'PY'\n" + strings.Repeat("p=p\n", 60000) + "PY",
		"python3 - <<'PY'\nwhile True:\n" + strings.Repeat("    p=p\n", 40000) + "PY",
		// A literal that doubles each line, and a very long attribute chain.
		"python3 -c \"p='ab'\n" + strings.Repeat("p=p+p+p+p+p+p+p+p\n", 12) + "print(open(p).read())\"",
		"python3 -c 'x" + strings.Repeat(".a", 100000) + "()'",
		// Deep nesting up to the size cap, twice: the command's deadline
		// covers both.
		strings.Repeat("python3 - <<'PY'\na='x'\n"+strings.Repeat(deep, 40)+"PY\n", 2),
		"python3 - <<'PY'\np='a.go'\n" + strings.Repeat(blocks.String(), 4) + "PY",
		// Thousands of backslash-continued statements, which the segment
		// splitter rewrites.
		"python3 -c 1\n" + strings.Repeat("echo a \\\nb\n", 50000),
		// A script that keeps the parser's error recovery busy.
		"python3 - <<'PY'\n" + strings.Repeat("a[b(c{d\n", 32000) + "PY",
	} {
		start := time.Now()
		enrichBash(map[string]any{"command": cmd}, "/repo")
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("a %d-byte script took %s", len(cmd), d)
		}
	}
}
