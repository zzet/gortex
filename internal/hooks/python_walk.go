package hooks

import (
	"path"
	"path/filepath"
	"strings"
	"time"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// python_walk.go finds the inline Python scripts that walk a directory and
// read the files the walk yields: the shape of Grep with a `path:` scope.
// It runs after every other answer and only ever adds a deny, so it denies
// only the loops agents write for this, matched whole:
//
//	for root, dirs, files in os.walk('internal'):   # or os.listdir, glob, rglob
//	    if '.git' in root: continue                   # prunes, optional
//	    for f in files:
//	        if f.endswith('.go'):                     # filter, optional
//	            p = os.path.join(root, f)
//	            with open(p) as fh:
//	                if 'X' in fh.read(): print(p)
//
// The file's name and path may only be built into the path, read, printed
// or tested by the filter; anything else, or a write to a path the hook
// cannot name anywhere in the script, leaves the walk with no opinion.

// pyWalk is a walk loop the template matched.
type pyWalk struct {
	root    string     // the literal root as written
	filters [][]string // each filter's admitted names (x.go, *.md); all must admit source
	read    bool       // the loop reads the files it yields
	shallow bool       // it yields only root's own entries (os.listdir, a glob without **)
}

// denies reports whether the walk reads source: every filter admits some
// source name. A glob of bare wildcards admits everything; one naming
// files with no extension (`README*`) admits no source.
func (w *pyWalk) denies() bool {
	if w.root == "" || !w.read {
		return false
	}
	for _, names := range w.filters {
		admits := false
		for _, name := range names {
			if strings.ContainsAny(name, "*?[") {
				for _, alt := range globAlternatives(name) {
					admits = admits || strings.Trim(alt, "*") == "" || looksLikeSourceFile(alt)
				}
			} else {
				admits = admits || looksLikeSourceFile(name)
			}
		}
		if !admits {
			return false
		}
	}
	return true
}

// pyHandleReaders read the content of a file handle passed to them.
var pyHandleReaders = map[string]bool{"json.load": true, "csv.reader": true, "csv.DictReader": true,
	"enumerate": true, "list": true, "sorted": true, "yaml.safe_load": true, "tomllib.load": true}

// analyzePythonWalks returns the walk loops of one script. ok is false when
// the script does not parse or exceeds the budget. A script that names no
// walk is not parsed.
func analyzePythonWalks(code string, deadline time.Time) ([]*pyWalk, bool) {
	if !strings.Contains(code, "walk") && !strings.Contains(code, "glob") && !strings.Contains(code, "listdir") {
		return nil, true
	}
	a, done, ok := newPyAnalysis(code, deadline, true) // the extra analysis' import bookkeeping
	if !ok {
		return nil, false
	}
	defer done()
	var loops []*sitter.Node
	a.literals = map[string]string{}
	writes := a.scanWalks(a.tree, &loops, 0)
	var walks []*pyWalk
	for _, loop := range loops {
		walks = append(walks, a.walkLoop(loop)...)
	}
	if a.aborted {
		return nil, false
	}
	// A script that writes a path it builds may be writing back what it
	// read: an edit, not a search.
	if writes {
		return nil, true
	}
	return walks, true
}

// scanWalks collects the for loops under n, and reports whether the script
// writes a path that is not a literal.
func (a *pyAnalysis) scanWalks(n *sitter.Node, loops *[]*sitter.Node, depth int) bool {
	if !a.step(depth) {
		return false
	}
	writes := false
	switch n.Type() {
	case "for_statement":
		*loops = append(*loops, n)
	case "assignment":
		if left := n.ChildByFieldName("left"); left.Type() == "identifier" {
			if s, ok := a.stringLiteral(n.ChildByFieldName("right")); ok {
				a.literals[a.text(left)] = s
			}
		}
	case "call":
		name, method, object := a.callName(n.ChildByFieldName("function"))
		pos, kw := a.args(n)
		target, modeAt := object, 0
		switch {
		case strings.HasPrefix(name, "shutil.") || name == "os.replace" || name == "os.rename" || name == "os.open" ||
			name == "os.write" || name == "replace" || name == "rename" ||
			((method == "replace" || method == "rename") && len(pos) == 1):
			// Moves and low-level writes, `Path.replace(target)` — not
			// `str.replace(old, new)`.
			writes = true
		case method == "write_text" || method == "write_bytes":
			modeAt = -1
		case name == "open" || pyOpenModules[a.dotted(object)]:
			target, modeAt = kw["file"], 1
			if len(pos) > 0 {
				target = pos[0]
			}
		case method != "open" || name == "os.open":
			modeAt = -2
		}
		if modeAt > -2 {
			mode, known := "w", true
			if modeAt >= 0 {
				mode = ""
				if len(pos) > modeAt {
					mode, known = a.stringLiteral(pos[modeAt])
				}
				if kw["mode"] != nil {
					mode, known = a.stringLiteral(kw["mode"])
				}
			}
			_, lit := a.stringLiteral(target)
			writes = writes || ((!known || strings.ContainsAny(mode, "wax+")) && !lit)
		}
	}
	for c := range n.NamedChildren() {
		writes = a.scanWalks(c, loops, depth+1) || writes
	}
	return writes
}

// pyFile is what a file loop knows of the file it is on.
type pyFile struct {
	name    string          // the bare file name, or ""
	dir     string          // the directory it is in, or ""
	dirLit  string          // the literal directory it is in (os.listdir), or ""
	path    string          // the path, or ""
	derived map[string]bool // names assigned the path
	handles map[string]bool // names of handles open on it
}

// walkLoop matches one for loop against the templates.
func (a *pyAnalysis) walkLoop(loop *sitter.Node) []*pyWalk {
	iter := unparen(loop.ChildByFieldName("right"))
	target := loop.ChildByFieldName("left")
	if iter == nil || target == nil {
		return nil
	}
	var name string
	var pos []*sitter.Node
	var kw map[string]*sitter.Node
	if iter.Type() == "call" {
		name, _, _ = a.callName(iter.ChildByFieldName("function"))
		pos, kw = a.args(iter)
	}
	switch {
	case name == "os.walk" && a.realModule("os") && len(pos) == 1 && len(kw) == 0:
		return a.walkTree(loop, a.literal(pos[0]))
	case name == "os.listdir" && a.realModule("os") && len(pos) == 1 && target.Type() == "identifier" && a.once(target):
		root := a.literal(pos[0])
		return a.fileLoop(loop, []*pyWalk{{root: root, shallow: true}}, &pyFile{name: a.text(target), dirLit: root})
	case target.Type() == "identifier" && a.once(target):
		var walks []*pyWalk
		if !a.globWalks(iter, &walks, 0) {
			return nil
		}
		return a.fileLoop(loop, walks, &pyFile{path: a.text(target)})
	}
	return nil
}

// globWalks reads a glob loop's iterable: glob.glob / iglob of a literal
// pattern (recursive= allowed), Path(literal).glob / rglob of one, sorted()
// of one, or `+` of two.
func (a *pyAnalysis) globWalks(n *sitter.Node, walks *[]*pyWalk, depth int) bool {
	n = unparen(n)
	if depth > 4 || n == nil {
		return false
	}
	if n.Type() == "binary_operator" && a.text(n.ChildByFieldName("operator")) == "+" {
		return a.globWalks(n.ChildByFieldName("left"), walks, depth+1) && a.globWalks(n.ChildByFieldName("right"), walks, depth+1)
	}
	if n.Type() != "call" {
		return false
	}
	name, method, object := a.callName(n.ChildByFieldName("function"))
	pos, kw := a.args(n)
	if name == "sorted" && len(pos) == 1 && len(kw) == 0 && !a.shadowed("sorted") {
		return a.globWalks(pos[0], walks, depth+1)
	}
	if len(pos) != 1 {
		return false
	}
	for k := range kw {
		if k != "recursive" {
			return false // root_dir= and the like move the walk
		}
	}
	var root, pattern string
	switch {
	case (name == "glob.glob" || name == "glob.iglob") && a.realModule("glob"):
		pattern = a.literal(pos[0])
		root = globRoot(pattern)
	case (method == "glob" || method == "rglob") && object != nil && object.Type() == "call":
		pathName, _, _ := a.callName(object.ChildByFieldName("function"))
		args, _ := a.args(object)
		if (pathName != "Path" && pathName != "pathlib.Path") || len(args) != 1 {
			return false
		}
		root, pattern = a.literal(args[0]), a.literal(pos[0])
		if globRoot(pattern) == "" || (method == "rglob" && strings.Contains(pattern, "/")) {
			return false // no wildcard, or a directory in an rglob pattern
		}
		if prefix := globRoot(pattern); method == "glob" && prefix != "." {
			root = filepath.Join(root, prefix)
		}
	default:
		return false
	}
	// Python's glob does not expand braces, and a pattern naming
	// directories past its first wildcard walks only some of the tree.
	if root == "" || strings.Contains(pattern, "{") || namedDirs(pattern) {
		return false
	}
	shallow := method != "rglob" && !strings.Contains(pattern, "**")
	*walks = append(*walks, &pyWalk{root: root, filters: [][]string{{filepath.Base(pattern)}}, shallow: shallow})
	return true
}

// walkTree matches `for root, dirs, files in os.walk(…)`: prunes, then one
// `for f in files` loop.
func (a *pyAnalysis) walkTree(loop *sitter.Node, root string) []*pyWalk {
	target := loop.ChildByFieldName("left")
	if root == "" || target.Type() != "pattern_list" || target.NamedChildCount() != 3 {
		return nil
	}
	var names [3]string
	for i := 0; i < 3; i++ {
		c := target.NamedChild(i)
		if c.Type() != "identifier" || !a.once(c) {
			return nil
		}
		names[i] = a.text(c)
	}
	dir, dirs, files := names[0], names[1], names[2]
	var inner *sitter.Node
	for stmt := range loop.ChildByFieldName("body").NamedChildren() {
		switch {
		case stmt.Type() == "comment":
		case inner == nil && stmt.Type() == "for_statement" && a.text(stmt.ChildByFieldName("right")) == files &&
			stmt.ChildByFieldName("left").Type() == "identifier" && a.once(stmt.ChildByFieldName("left")):
			inner = stmt
		case stmt.Type() == "if_statement" && stmt.NamedChildCount() == 2 && onlyContinue(stmt.ChildByFieldName("consequence")) &&
			a.prune(stmt.ChildByFieldName("condition"), dir, 0):
		case a.mentions(stmt, dir, dirs, files):
			return nil
		}
	}
	if inner == nil || a.mentions(inner.ChildByFieldName("body"), dirs, files) {
		return nil
	}
	return a.fileLoop(inner, []*pyWalk{{root: root}}, &pyFile{name: a.text(inner.ChildByFieldName("left")), dir: dir})
}

// prune reports whether a condition skips some directories of the walk:
// `'.git' in root`, `root.startswith('…')`, `any(s in root for s in SKIP)`,
// and `or` / `and` of these.
func (a *pyAnalysis) prune(n *sitter.Node, dir string, depth int) bool {
	if n = unparen(n); n == nil || depth > 8 {
		return false
	}
	switch n.Type() {
	case "boolean_operator":
		return a.prune(n.ChildByFieldName("left"), dir, depth+1) && a.prune(n.ChildByFieldName("right"), dir, depth+1)
	case "comparison_operator":
		_, lit := a.stringLiteral(n.NamedChild(0))
		return n.ChildCount() == 3 && n.Child(1).Type() == "in" && lit && a.text(n.NamedChild(1)) == dir
	case "call":
		name, method, object := a.callName(n.ChildByFieldName("function"))
		if (method == "startswith" || method == "endswith") && object != nil && a.text(object) == dir {
			pos, _ := a.args(n)
			_, lit := a.stringLiteral(firstNode(pos))
			return len(pos) == 1 && lit
		}
		gen := n.ChildByFieldName("arguments")
		if name != "any" || a.shadowed("any") || gen == nil || gen.Type() != "generator_expression" || gen.NamedChildCount() != 2 {
			return false
		}
		test, clause := unparen(gen.ChildByFieldName("body")), gen.NamedChild(1)
		names := unparen(clause.ChildByFieldName("right"))
		_, lits := a.literalSet(names)
		return test != nil && test.Type() == "comparison_operator" && test.ChildCount() == 3 && test.Child(1).Type() == "in" &&
			a.text(test.NamedChild(1)) == dir && a.text(test.NamedChild(0)) == a.text(clause.ChildByFieldName("left")) &&
			names != nil && ((names.Type() == "identifier" && a.once(names)) || lits)
	}
	return false
}

// fileLoop matches the loop over the files of a walk: an optional filter,
// then a body that uses the file only as fileUse allows. walks are the
// walks the loop's files come from; more than one for `glob(a) + glob(b)`.
func (a *pyAnalysis) fileLoop(loop *sitter.Node, walks []*pyWalk, f *pyFile) []*pyWalk {
	body := loop.ChildByFieldName("body")
	f.derived, f.handles = map[string]bool{}, map[string]bool{}
	a.fileNames(body, f, 0)
	// The filter is the one statement of the body, an `if` with no else,
	// or a leading `if not …: continue`. A test that does not look at the
	// file filters nothing it reads.
	var test *sitter.Node
	var stmts []*sitter.Node
	prunes := map[uintptr]bool{}
	for c := range body.NamedChildren() {
		switch {
		case c.Type() == "comment":
		case f.dir != "" && c.Type() == "if_statement" && c.NamedChildCount() == 2 &&
			onlyContinue(c.ChildByFieldName("consequence")) && a.prune(c.ChildByFieldName("condition"), f.dir, 0):
			prunes[c.Id()] = true // `if '.git' in root: continue`
		default:
			stmts = append(stmts, c)
		}
	}
	switch first := firstNode(stmts); {
	case first == nil || first.Type() != "if_statement" || first.NamedChildCount() != 2:
	case len(stmts) == 1:
		test = first.ChildByFieldName("condition")
	case onlyContinue(first.ChildByFieldName("consequence")) && unparen(first.ChildByFieldName("condition")).Type() == "not_operator":
		test = unparen(first.ChildByFieldName("condition")).ChildByFieldName("argument")
	}
	if test != nil && a.mentions(test, f.all()...) {
		names, ok := a.fileFilter(test, f, 0)
		if !ok {
			return nil
		}
		for _, w := range walks {
			w.filters = append(w.filters, names)
		}
	}
	for c := range body.NamedChildren() {
		if !prunes[c.Id()] && !a.usesOnly(c, test, f, 0) {
			return nil
		}
	}
	read := a.reads(body, f, 0)
	for _, w := range walks {
		w.read = read
	}
	return walks
}

// all lists the names that hold the file or its path.
func (f *pyFile) all() []string {
	out := []string{f.name, f.dir, f.path}
	for n := range f.derived {
		out = append(out, n)
	}
	return out
}

// fileNames records the names a loop body assigns the file's path to, and
// the handles it opens on it. A name bound more than once may hold
// anything.
func (a *pyAnalysis) fileNames(n *sitter.Node, f *pyFile, depth int) {
	if !a.step(depth) || depth > pyMaxDepth {
		return
	}
	switch n.Type() {
	case "assignment":
		left, right := n.ChildByFieldName("left"), n.ChildByFieldName("right")
		if left.Type() == "identifier" && right != nil && a.bindingCount(a.text(left)) == 1 {
			if a.filePath(right, f, 0) {
				f.derived[a.text(left)] = true
			} else if a.openOn(right, f) {
				f.handles[a.text(left)] = true
			}
		}
	case "as_pattern":
		alias := n.ChildByFieldName("alias")
		if alias != nil && a.openOn(n.NamedChild(0), f) && a.bindingCount(strings.TrimSpace(a.text(alias))) == 1 {
			f.handles[strings.TrimSpace(a.text(alias))] = true
		}
	}
	for c := range n.NamedChildren() {
		a.fileNames(c, f, depth+1)
	}
}

// openOn reports whether n opens the file for reading: open(path) or
// path.open() with no write mode.
func (a *pyAnalysis) openOn(n *sitter.Node, f *pyFile) bool {
	if n = unparen(n); n == nil || n.Type() != "call" {
		return false
	}
	name, method, object := a.callName(n.ChildByFieldName("function"))
	pos, kw := a.args(n)
	modeAt := 1
	switch {
	case hasSplat(pos):
		return false
	case name == "open" && !a.shadowed("open") && len(pos) > 0 && a.filePath(pos[0], f, 0):
	case method == "open" && name != "os.open" && object != nil && a.filePath(object, f, 0):
		modeAt = 0
	default:
		return false
	}
	for k := range kw {
		if k != "mode" && k != "encoding" && k != "errors" && k != "newline" && k != "buffering" {
			return false // dir_fd= and opener= open something else
		}
	}
	mode := ""
	if len(pos) > modeAt {
		mode, _ = a.stringLiteral(pos[modeAt])
	}
	if kw["mode"] != nil {
		mode, _ = a.stringLiteral(kw["mode"])
	}
	return !strings.ContainsAny(mode, "wax+")
}

// filePath reports whether n is the file's path: the path name or one it
// was assigned to, os.path.join(dir, name), dir + '/' + name, an f-string
// `{dir}/{name}`, '%s/%s' % (dir, name), Path(dir) / name, or str() /
// Path() of one. For os.listdir the literal root stands for dir.
func (a *pyAnalysis) filePath(n *sitter.Node, f *pyFile, depth int) bool {
	if n = unparen(n); n == nil || depth > 4 {
		return false
	}
	isName := func(c *sitter.Node) bool { return f.name != "" && a.text(unparen(c)) == f.name }
	switch n.Type() {
	case "identifier":
		return (f.path != "" && a.text(n) == f.path) || f.derived[a.text(n)]
	case "call":
		name, _, _ := a.callName(n.ChildByFieldName("function"))
		pos, kw := a.args(n)
		switch {
		case len(kw) > 0:
		case name == "os.path.join" && len(pos) == 2:
			return a.isDir(pos[0], f, depth+1) && isName(pos[1])
		case (name == "str" || name == "Path" || name == "pathlib.Path") && len(pos) == 1:
			return a.filePath(pos[0], f, depth+1)
		}
	case "binary_operator":
		left, right := unparen(n.ChildByFieldName("left")), n.ChildByFieldName("right")
		switch a.text(n.ChildByFieldName("operator")) {
		case "/":
			return a.isDir(left, f, depth+1) && isName(right)
		case "+":
			if left.Type() == "binary_operator" && a.text(left.ChildByFieldName("operator")) == "+" {
				sep, _ := a.stringLiteral(left.ChildByFieldName("right"))
				return sep == "/" && a.isDir(left.ChildByFieldName("left"), f, depth+1) && isName(right)
			}
			if lit, ok := a.stringLiteral(left); ok && f.dirLit != "" {
				return strings.HasSuffix(lit, "/") && filepath.Clean(lit) == filepath.Clean(f.dirLit) && isName(right)
			}
		case "%":
			format, ok := a.stringLiteral(left)
			right = unparen(right)
			switch {
			case !ok:
			case format == "%s/%s" && right.Type() == "tuple" && right.NamedChildCount() == 2:
				return a.isDir(right.NamedChild(0), f, depth+1) && isName(right.NamedChild(1))
			case f.dirLit != "" && strings.HasSuffix(format, "/%s") && strings.Count(format, "%") == 1:
				return filepath.Clean(strings.TrimSuffix(format, "%s")) == filepath.Clean(f.dirLit) && isName(right)
			}
		}
	case "string":
		// f'{dir}/{name}', and nothing else in it.
		var parts []*sitter.Node
		for c := range n.NamedChildren() {
			if c.Type() != "string_start" && c.Type() != "string_end" {
				parts = append(parts, c)
			}
		}
		return len(parts) == 3 && parts[0].Type() == "interpolation" && a.isDir(parts[0].NamedChild(0), f, depth+1) &&
			parts[1].Type() == "string_content" && a.text(parts[1]) == "/" &&
			parts[2].Type() == "interpolation" && isName(parts[2].NamedChild(0))
	}
	return false
}

// isDir reports whether n is the directory the file is in.
func (a *pyAnalysis) isDir(n *sitter.Node, f *pyFile, depth int) bool {
	if n = unparen(n); n == nil || depth > 4 {
		return false
	}
	if lit := a.literal(n); lit != "" && f.dirLit != "" {
		return filepath.Clean(lit) == filepath.Clean(f.dirLit)
	}
	if n.Type() == "call" {
		name, _, _ := a.callName(n.ChildByFieldName("function"))
		pos, kw := a.args(n)
		return (name == "Path" || name == "pathlib.Path") && len(pos) == 1 && len(kw) == 0 && a.isDir(pos[0], f, depth+1)
	}
	return f.dir != "" && a.text(n) == f.dir
}

// fileFilter reads a filter on the file: `f.endswith(…)` (a suffix or a
// tuple of them, through lower()/upper()/casefold() and `.name`), `or` of
// such tests, or one `and` a test that does not look at the file's name
// (`'X' in open(p).read()`).
func (a *pyAnalysis) fileFilter(n *sitter.Node, f *pyFile, depth int) ([]string, bool) {
	if n = unparen(n); n == nil || depth > 8 {
		return nil, false
	}
	switch n.Type() {
	case "boolean_operator":
		l, r := n.ChildByFieldName("left"), n.ChildByFieldName("right")
		left, lok := a.fileFilter(l, f, depth+1)
		right, rok := a.fileFilter(r, f, depth+1)
		switch {
		case a.text(n.ChildByFieldName("operator")) == "or":
			return append(left, right...), lok && rok
		case lok && a.contentOnly(r, f):
			return left, true
		case rok && a.contentOnly(l, f):
			return right, true
		}
		return nil, false
	case "call":
		_, method, object := a.callName(n.ChildByFieldName("function"))
		pos, kw := a.args(n)
		if method != "endswith" || len(pos) != 1 || len(kw) > 0 || !a.fileSubject(object, f) {
			return nil, false
		}
		suffixes := []*sitter.Node{unparen(pos[0])}
		if suffixes[0].Type() == "tuple" {
			suffixes = suffixes[:0]
			for c := range unparen(pos[0]).NamedChildren() {
				suffixes = append(suffixes, c)
			}
		}
		var names []string
		for _, s := range suffixes {
			lit, ok := a.stringLiteral(s)
			if !ok || lit == "" || strings.Contains(lit, "/") {
				return nil, false
			}
			names = append(names, "x"+lit)
		}
		return names, len(names) > 0
	}
	return nil, false
}

// contentOnly reports whether a test looks at the file only through reads
// of it.
func (a *pyAnalysis) contentOnly(n *sitter.Node, f *pyFile) bool {
	if !a.mentions(n, f.all()...) {
		return true
	}
	if n.Type() == "identifier" {
		return false
	}
	if n.Type() == "call" && ((a.openOn(n, f) && a.consumed(n)) || a.readCallOn(n, f)) {
		return true
	}
	for c := range n.NamedChildren() {
		if !a.contentOnly(c, f) {
			return false
		}
	}
	return true
}

// readCallOn reports whether n is path.read_text() or read_bytes().
func (a *pyAnalysis) readCallOn(n *sitter.Node, f *pyFile) bool {
	_, method, object := a.callName(n.ChildByFieldName("function"))
	return (method == "read_text" || method == "read_bytes") && a.filePath(object, f, 0)
}

// fileSubject reports whether n is the file's name or path, through
// lower()/upper()/casefold() and `.name`.
func (a *pyAnalysis) fileSubject(n *sitter.Node, f *pyFile) bool {
	for i := 0; i < 4 && n != nil; i++ {
		switch n = unparen(n); n.Type() {
		case "identifier":
			return (f.name != "" && a.text(n) == f.name) || (f.path != "" && a.text(n) == f.path) || f.derived[a.text(n)]
		case "attribute":
			if a.text(n.ChildByFieldName("attribute")) != "name" {
				return false
			}
			n = n.ChildByFieldName("object")
		case "call":
			_, method, object := a.callName(n.ChildByFieldName("function"))
			if pos, kw := a.args(n); len(pos)+len(kw) > 0 || (method != "lower" && method != "upper" && method != "casefold") {
				return false
			}
			n = object
		default:
			return false
		}
	}
	return false
}

// usesOnly reports whether every use of the file's names in n is one the
// template allows (see fileUse).
func (a *pyAnalysis) usesOnly(n, test *sitter.Node, f *pyFile, depth int) bool {
	if !a.step(depth) || depth > pyMaxDepth {
		return false
	}
	if n.Type() == "identifier" {
		name := a.text(n)
		if f.handles[name] {
			// A handle is read, iterated, handed to a reader or closed;
			// `fh.name` and the like are the path again.
			p := n.Parent()
			if p.Type() == "as_pattern_target" || (p.Type() == "assignment" && sameNode(p.ChildByFieldName("left"), n)) {
				return true
			}
			return a.consumed(n) || (p.Type() == "attribute" && a.text(p.ChildByFieldName("attribute")) == "close")
		}
		if name != f.name && name != f.dir && name != f.path && !f.derived[name] {
			return true
		}
		return a.fileUse(n, test, f)
	}
	for c := range n.NamedChildren() {
		if !a.usesOnly(c, test, f, depth+1) {
			return false
		}
	}
	return true
}

// fileUse reports whether one use of the file's name or path is one the
// template allows: inside the filter test, inside print, the loop target,
// or part of the path (see filePath) where that path is assigned to a name
// fileNames recorded, opened for reading or read.
func (a *pyAnalysis) fileUse(n, test *sitter.Node, f *pyFile) bool {
	if test != nil && n.StartByte() >= test.StartByte() && n.EndByte() <= test.EndByte() {
		return true // the filter, which fileFilter has read
	}
	if p := n.Parent(); p != nil && p.Type() == "for_statement" && sameNode(p.ChildByFieldName("left"), n) {
		return true // the loop target
	}
	// The widest path around it, a few levels up at most.
	top := n
	for p, i := n.Parent(), 0; p != nil && i < 6; p, i = p.Parent(), i+1 {
		if a.filePath(p, f, 0) {
			top = p
		}
	}
	for p, i := top.Parent(), 0; p != nil && i < 64; p, i = p.Parent(), i+1 {
		switch p.Type() {
		case "parenthesized_expression", "argument_list":
			if p.Type() == "argument_list" {
				call := p.Parent()
				if name, _, _ := a.callName(call.ChildByFieldName("function")); name == "print" && !a.shadowed("print") {
					return true
				}
				return sameNode(top, firstNode(argsOf(a, call))) && a.openOn(call, f)
			}
			top = p
			continue
		case "assignment":
			left := p.ChildByFieldName("left")
			return (sameNode(p.ChildByFieldName("right"), top) && a.filePath(top, f, 0) && f.derived[a.text(left)]) ||
				(sameNode(left, top) && f.derived[a.text(top)])
		case "attribute":
			attr := a.text(p.ChildByFieldName("attribute"))
			return sameNode(p.ChildByFieldName("object"), top) && a.filePath(top, f, 0) &&
				(attr == "read_text" || attr == "read_bytes" || attr == "open") && a.openOrRead(p.Parent(), f)
		}
		// Anything else only as plain string building inside print: no test
		// on it there (`… if 'docs' in r else ''`) decides what is read.
		for q, j := p, 0; q != nil && j < 64; q, j = q.Parent(), j+1 {
			switch q.Type() {
			case "binary_operator", "interpolation", "string", "concatenated_string", "parenthesized_expression",
				"argument_list", "keyword_argument", "expression_list", "tuple":
			case "call":
				name, _, _ := a.callName(q.ChildByFieldName("function"))
				if name == "print" && !a.shadowed("print") {
					return true
				}
				if name != "str" && name != "repr" && !strings.HasPrefix(name, "os.path.") {
					return false
				}
			default:
				return false
			}
		}
		return false
	}
	return false
}

// argsOf returns a call's positional arguments.
func argsOf(a *pyAnalysis, call *sitter.Node) []*sitter.Node {
	pos, _ := a.args(call)
	return pos
}

// openOrRead reports whether a path method call opens the file for reading
// or reads it.
func (a *pyAnalysis) openOrRead(n *sitter.Node, f *pyFile) bool {
	return n != nil && n.Type() == "call" && (a.openOn(n, f) || a.readCallOn(n, f))
}

// reads reports whether n reads the file's content: open(path) or
// path.open() whose handle is read, iterated or handed to a reader —
// directly or through its with/assigned name — or path.read_text().
// A discarded syntax check (`ast.parse(open(p).read())` as a statement)
// is not a read, as for the read door.
func (a *pyAnalysis) reads(n *sitter.Node, f *pyFile, depth int) bool {
	if !a.step(depth) || depth > pyMaxDepth {
		return false
	}
	switch n.Type() {
	case "call":
		if (a.readCallOn(n, f) && !a.discarded(n)) || (a.openOn(n, f) && !a.discardedParseCheck(n) && !a.discardedOpen(n) && a.consumed(n)) {
			return true
		}
	case "identifier":
		if f.handles[a.text(n)] && a.consumed(n) && !a.discarded(n.Parent().Parent()) {
			return true
		}
	}
	for c := range n.NamedChildren() {
		if a.reads(c, f, depth+1) {
			return true
		}
	}
	return false
}

// discarded reports whether a read is only syntax-checked and thrown away:
// `ast.parse(fh.read())` or `compile(p.read_text(), …)` as a statement, or
// assigned to `_`.
func (a *pyAnalysis) discarded(read *sitter.Node) bool {
	args := read.Parent()
	if read.Type() != "call" || args == nil || args.Type() != "argument_list" {
		return false
	}
	check := args.Parent()
	name, _, _ := a.callName(check.ChildByFieldName("function"))
	stmt := check.Parent()
	if stmt != nil && stmt.Type() == "assignment" && a.text(stmt.ChildByFieldName("left")) == "_" && sameNode(stmt.ChildByFieldName("right"), check) {
		stmt = stmt.Parent()
	}
	return (name == "ast.parse" || name == "compile" || name == "parse") && stmt != nil && stmt.Type() == "expression_statement"
}

// discardedOpen is discarded for `open(p).read()`.
func (a *pyAnalysis) discardedOpen(open *sitter.Node) bool {
	attr := open.Parent()
	if attr == nil || attr.Type() != "attribute" || a.text(attr.ChildByFieldName("attribute")) != "read" {
		return false
	}
	read := attr.Parent()
	return read != nil && read.Type() == "call" && a.discarded(read)
}

// consumed reports whether the handle at n has its content read: `.read()`
// and the like on it, iterating it, or handing it to a reader.
func (a *pyAnalysis) consumed(n *sitter.Node) bool {
	p := n.Parent()
	for p != nil && p.Type() == "parenthesized_expression" {
		n, p = p, p.Parent()
	}
	switch {
	case p == nil:
		return false
	case p.Type() == "attribute":
		attr := a.text(p.ChildByFieldName("attribute"))
		return sameNode(p.ChildByFieldName("object"), n) && (attr == "read" || attr == "readline" || attr == "readlines")
	case p.Type() == "argument_list":
		name, _, _ := a.callName(p.Parent().ChildByFieldName("function"))
		return pyHandleReaders[name]
	case p.Type() == "for_statement" || p.Type() == "for_in_clause":
		return sameNode(p.ChildByFieldName("right"), n)
	}
	return false
}

// mentions reports whether any of names appears in n as a name.
func (a *pyAnalysis) mentions(n *sitter.Node, names ...string) bool {
	found := false
	var visit func(c *sitter.Node, depth int)
	visit = func(c *sitter.Node, depth int) {
		if found || c == nil || !a.step(depth) || depth > pyMaxDepth {
			return
		}
		if c.Type() == "identifier" {
			if p := c.Parent(); p != nil && p.Type() == "attribute" && sameNode(p.ChildByFieldName("attribute"), c) {
				return
			}
			for _, name := range names {
				found = found || (name != "" && a.text(c) == name)
			}
			return
		}
		for cc := range c.NamedChildren() {
			visit(cc, depth+1)
		}
	}
	visit(n, 0)
	return found
}

// once reports whether the name at n is bound only there.
func (a *pyAnalysis) once(n *sitter.Node) bool { return a.bindingCount(a.text(n)) == 1 }

// literal is the literal string n holds: a string, or a name bound once,
// by an assignment of a string (scanWalks records them).
func (a *pyAnalysis) literal(n *sitter.Node) string {
	if s, ok := a.stringLiteral(n); ok {
		return s
	}
	if n.Type() == "identifier" && a.once(n) {
		return a.literals[a.text(n)]
	}
	return ""
}

// literalSet reads a tuple, list or set of string literals.
func (a *pyAnalysis) literalSet(n *sitter.Node) ([]string, bool) {
	if n == nil || (n.Type() != "tuple" && n.Type() != "list" && n.Type() != "set") {
		return nil, false
	}
	var out []string
	for c := range n.NamedChildren() {
		s, ok := a.stringLiteral(c)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// onlyContinue reports whether a block is a single continue.
func onlyContinue(body *sitter.Node) bool {
	return body != nil && body.NamedChildCount() == 1 && body.NamedChild(0).Type() == "continue_statement"
}

// namedDirs reports whether a glob pattern names directories past its
// first wildcard (`**/docs/*`): it walks only some of the tree.
func namedDirs(pattern string) bool {
	i := strings.IndexAny(pattern, "*?[")
	if i < 0 {
		return false
	}
	for _, d := range strings.Split(path.Dir(pattern[strings.LastIndex(pattern[:i], "/")+1:]), "/") {
		if d != "." && strings.Trim(d, "*") != "" {
			return true
		}
	}
	return false
}

// globRoot is the literal directory a glob pattern starts in, or "" for a
// pattern with no wildcard, which walks nothing.
func globRoot(pattern string) string {
	if !strings.ContainsAny(pattern, "*?[{") {
		return ""
	}
	return globLiteralPrefix(filepath.FromSlash(pattern)) // Python takes / on every OS
}

// unparen looks through parentheses.
func unparen(n *sitter.Node) *sitter.Node {
	for n != nil && n.Type() == "parenthesized_expression" {
		n = n.NamedChild(0)
	}
	return n
}

// firstNode returns the first node, or nil.
func firstNode(ns []*sitter.Node) *sitter.Node {
	if len(ns) == 0 {
		return nil
	}
	return ns[0]
}

// sameNode reports whether n is m.
func sameNode(n, m *sitter.Node) bool { return n != nil && m != nil && n.Id() == m.Id() }
