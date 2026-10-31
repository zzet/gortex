package hooks

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	pylang "github.com/zzet/gortex/internal/parser/tsitter/python"
)

// python_script_facts.go reads one inline Python script with the tree-sitter
// Python grammar and reports the files it reads and writes by literal path.
// Calls, attributes, strings and bindings come from the syntax tree, and
// names resolve by scope and position, so a path is followed through
// `p = 'x.py'; open(p)` but `os.path` is never a variable named `path`, a
// path inside a string literal is not a read, and a name rebound before the
// read holds what it was rebound to.

// pyScriptFacts is what the analysis learns from one script.
type pyScriptFacts struct {
	reads     []string        // literal source paths read, in order
	writeBack map[string]bool // literal paths written back
	guessed   map[string]bool // written back only by a write it cannot name
	seen      map[string]bool
	osReads   []string // literal source paths os.open opens read-only
	writes    []string // literal paths os.open opens for writing
	commands  []string // literal command lines the script runs (os.system, subprocess)
}

// pyMaxSteps bounds the analysis of one script and pyMaxDepth its expression
// nesting; past either, or past the caller's deadline, the hook has no
// opinion.
const (
	pyMaxSteps   = 200_000
	pyMaxDepth   = 200
	pyMaxLiteral = 4096 // a longer value is no path; past it nothing resolves
)

type pyBindingKey struct {
	name  string
	scope uintptr
}

type pyBinding struct {
	pos uint32
	lit string // the literal path the name holds, or ""
}

type pyAnalysis struct {
	src        []byte
	bindings   map[pyBindingKey][]pyBinding
	scopeOf    map[uintptr]uintptr
	compr      map[uintptr]bool
	classes    map[uintptr]bool
	ambiguous  map[string]bool
	bound      map[string]bool // every name bound anywhere, built on first use
	osImports  map[string]bool // names imported by `from os import NAME`
	imports    map[string]int  // bindings of a name by a plain `import name`
	fromNames  map[string]int  // bindings of a name as part of `from module import …`
	bindCount  map[string]int  // bindings of a name in any scope, built on first use
	osStar     bool            // `from os import *`
	extra      bool            // collect what only the extra analysis answers
	loopEnd    uint32          // end of the outermost loop around the node collect is at
	deadline   time.Time
	root       uintptr
	steps      int
	aborted    bool
	unresolved bool // writes a target it cannot name
	facts      pyScriptFacts
}

var (
	pyScopeTypes = map[string]bool{
		"function_definition": true, "lambda": true, "class_definition": true,
		"list_comprehension": true, "set_comprehension": true,
		"dictionary_comprehension": true, "generator_expression": true,
	}
	pyComprehensionTypes = map[string]bool{
		"list_comprehension": true, "set_comprehension": true,
		"dictionary_comprehension": true, "generator_expression": true,
	}
	// pyPathCalls pass a literal path through, joining their arguments.
	pyPathCalls = map[string]bool{"os.path.join": true, "os.path.abspath": true, "os.path.realpath": true,
		"os.path.normpath": true, "os.fspath": true, "str": true,
		"Path": true, "pathlib.Path": true, "PurePath": true, "pathlib.PurePath": true}
	pyOpenModules = map[string]bool{"io": true, "codecs": true, "builtins": true}
	// pyCommandCalls run a command line, or an argument vector as a list.
	pyCommandCalls = map[string]bool{"os.system": true, "os.popen": true,
		"subprocess.run": true, "subprocess.call": true, "subprocess.check_call": true,
		"subprocess.check_output": true, "subprocess.Popen": true,
		"subprocess.getoutput": true, "subprocess.getstatusoutput": true}
)

// analyzePythonScript parses one script and collects what it reads and
// writes. ok is false when the script does not parse cleanly or exceeds the
// analysis budget. With extra it also collects os.open accesses and the
// command lines the script runs; without, it does no work for them at all,
// so the reads and edits the hook has always answered cost what they did.
func analyzePythonScript(code string, deadline time.Time, extra bool) (pyScriptFacts, bool) {
	if len(code) > maxInlineScript || !time.Now().Before(deadline) {
		return pyScriptFacts{}, false
	}
	src := []byte(code)
	if parser.LooksBinary(src) {
		return pyScriptFacts{}, false
	}
	// The parse runs under the same deadline as the analysis. A parser
	// whose parse was cancelled cannot be reused, so this one is not pooled.
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	p := sitter.NewParser()
	defer p.Close()
	p.SetLanguage(pylang.GetLanguage())
	tree, err := p.ParseCtx(ctx, nil, src)
	if err != nil || tree == nil {
		return pyScriptFacts{}, false
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil || root.HasError() {
		return pyScriptFacts{}, false
	}
	a := &pyAnalysis{
		src:       src,
		bindings:  map[pyBindingKey][]pyBinding{},
		scopeOf:   map[uintptr]uintptr{},
		compr:     map[uintptr]bool{},
		classes:   map[uintptr]bool{},
		ambiguous: map[string]bool{},
		osImports: map[string]bool{},
		imports:   map[string]int{},
		fromNames: map[string]int{},
		deadline:  deadline,
		extra:     extra,
		root:      root.Id(),
		facts:     pyScriptFacts{writeBack: map[string]bool{}, guessed: map[string]bool{}, seen: map[string]bool{}},
	}
	a.bind(root, a.root, 0)
	a.collect(root, a.root, 0)
	if a.aborted {
		return pyScriptFacts{}, false
	}
	// A write the script cannot name may be any file it read.
	if a.unresolved {
		for _, p := range a.facts.reads {
			if !a.facts.writeBack[p] {
				a.facts.writeBack[p] = true
				a.facts.guessed[p] = true
			}
		}
	}
	return a.facts, true
}

// step charges one unit of work; past the budget the analysis aborts.
func (a *pyAnalysis) step(depth int) bool {
	a.steps++
	if a.steps > pyMaxSteps || depth > pyMaxDepth*4 ||
		(a.steps%1024 == 0 && !time.Now().Before(a.deadline)) {
		a.aborted = true
	}
	return !a.aborted
}

func (a *pyAnalysis) text(n *sitter.Node) string { return n.Content(a.src) }

// enterScope returns the scope n opens, or scope when it opens none.
func (a *pyAnalysis) enterScope(n *sitter.Node, scope uintptr) uintptr {
	if !pyScopeTypes[n.Type()] {
		return scope
	}
	id := n.Id()
	a.scopeOf[id] = scope
	if pyComprehensionTypes[n.Type()] {
		a.compr[id] = true
	}
	if n.Type() == "class_definition" {
		a.classes[id] = true
	}
	if t := n.Type(); t == "function_definition" || t == "class_definition" {
		if body := n.ChildByFieldName("body"); body != nil {
			a.markGlobals(body)
		}
	}
	return id
}

// markGlobals records the names a function or class body declares global or
// nonlocal, at any depth of its own blocks (nested functions and classes are
// scanned when they are entered). Such a name changes at a time the hook
// cannot order, so it resolves to nothing.
func (a *pyAnalysis) markGlobals(n *sitter.Node) {
	for c := range n.NamedChildren() {
		if !a.step(0) {
			return
		}
		switch c.Type() {
		case "global_statement", "nonlocal_statement":
			for name := range c.NamedChildren() {
				if name.Type() == "identifier" {
					a.ambiguous[a.text(name)] = true
				}
			}
		case "function_definition", "class_definition", "lambda":
		default:
			a.markGlobals(c)
		}
	}
}

// bind records every binding in source order. A comprehension's `for`
// clauses bind before its body, which reads them.
func (a *pyAnalysis) bind(n *sitter.Node, scope uintptr, depth int) {
	if !a.step(depth) {
		return
	}
	if t := n.Type(); t == "function_definition" || t == "class_definition" {
		if name := n.ChildByFieldName("name"); name != nil {
			a.record(a.text(name), n.StartByte(), scope, "")
		}
	}
	scope = a.enterScope(n, scope)
	switch n.Type() {
	case "list_comprehension", "set_comprehension", "dictionary_comprehension", "generator_expression":
		for c := range n.NamedChildren() {
			if c.Type() == "for_in_clause" {
				a.bind(c, scope, depth+1)
			}
		}
		for c := range n.NamedChildren() {
			if c.Type() != "for_in_clause" {
				a.bind(c, scope, depth+1)
			}
		}
		return
	case "for_statement", "for_in_clause":
		// Loop variables hold whatever the iterable yields: no literal path.
		a.bindTargets(n.ChildByFieldName("left"), scope, "")
	case "assignment", "named_expression":
		left, right := n.ChildByFieldName("left"), n.ChildByFieldName("right")
		if n.Type() == "named_expression" {
			left, right = n.ChildByFieldName("name"), n.ChildByFieldName("value")
		}
		// The name takes the value at the end of the statement, so its own
		// right-hand side still sees the previous binding. A walrus inside a
		// comprehension binds the scope around it.
		target := scope
		for n.Type() == "named_expression" && a.compr[target] {
			target = a.scopeOf[target]
		}
		if left != nil && left.Type() == "identifier" {
			a.record(a.text(left), n.EndByte(), target, a.eval(right, scope, 0))
		} else {
			a.bindTargets(left, scope, "")
		}
	case "augmented_assignment":
		a.bindTargets(n.ChildByFieldName("left"), scope, "")
	case "import_statement", "import_from_statement":
		if a.extra {
			a.recordImport(n)
		}
	case "parameters", "lambda_parameters":
		// Parameters hold no path. Their defaults run where the function is
		// written, so a walrus in one binds the scope around it.
		a.bindTargets(n, scope, "")
		for c := range n.NamedChildren() {
			a.bind(c, a.scopeOf[scope], depth+1)
		}
		return
	case "as_pattern_target", "dotted_name", "aliased_import":
		// File handles and imports hold no path.
		a.bindTargets(n, scope, "")
		return
	case "case_pattern", "delete_statement", "type_alias_statement":
		// A match capture, a del or a type alias: every name in it may now
		// hold anything, or nothing.
		a.bindAll(n, scope, depth)
		return
	case "type_parameter":
		a.bindAll(n, scope, depth)
		for c := range n.NamedChildren() {
			if c.Type() == "identifier" {
				a.ambiguous[a.text(c)] = true
			}
		}
		return
	}
	// A class's bases and keywords and a return annotation run where the
	// definition is written, so a walrus in them binds the scope around it.
	var bases, returns *sitter.Node
	if pyScopeTypes[n.Type()] {
		bases, returns = n.ChildByFieldName("superclasses"), n.ChildByFieldName("return_type")
	}
	for c := range n.NamedChildren() {
		s := scope
		if (bases != nil && c.Id() == bases.Id()) || (returns != nil && c.Id() == returns.Id()) {
			s = a.scopeOf[scope]
		}
		a.bind(c, s, depth+1)
	}
}

// bindTargets records every name a target pattern binds.
func (a *pyAnalysis) bindTargets(n *sitter.Node, scope uintptr, lit string) {
	if n == nil {
		return
	}
	if n.Type() == "identifier" {
		a.record(a.text(n), n.StartByte(), scope, lit)
		return
	}
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "identifier", "pattern_list", "tuple_pattern", "list_pattern", "parenthesized_expression",
			"tuple", "list", "as_pattern_target", "typed_parameter", "list_splat_pattern",
			"dictionary_splat_pattern":
			a.bindTargets(c, scope, "")
		case "default_parameter", "typed_default_parameter":
			// Only the name binds; the default is a value read outside.
			a.bindTargets(c.ChildByFieldName("name"), scope, "")
		}
	}
}

// bindAll records every name under n as bound to no literal path.
func (a *pyAnalysis) bindAll(n *sitter.Node, scope uintptr, depth int) {
	if !a.step(depth) {
		return
	}
	if n.Type() == "identifier" {
		a.record(a.text(n), n.StartByte(), scope, "")
		return
	}
	for c := range n.NamedChildren() {
		a.bindAll(c, scope, depth+1)
	}
}

// record adds a binding, keeping each name's bindings in a scope sorted by
// position (source order already, so the append is the common case).
func (a *pyAnalysis) record(name string, pos uint32, scope uintptr, lit string) {
	key := pyBindingKey{name, scope}
	bs := a.bindings[key]
	i := len(bs)
	for i > 0 && bs[i-1].pos > pos {
		i--
	}
	bs = append(bs, pyBinding{})
	copy(bs[i+1:], bs[i:])
	bs[i] = pyBinding{pos: pos, lit: lit}
	a.bindings[key] = bs
}

// lookup resolves name at pos: the nearest scope out from scope that binds
// it, and in that scope the latest binding before pos — or, in a
// comprehension, its own clause variable wherever it is used.
func (a *pyAnalysis) lookup(name string, pos uint32, scope uintptr) string {
	if a.ambiguous[name] {
		return ""
	}
	// Leaving a function or lambda body, the name is read when the function
	// runs, not where it is written: only a name bound once out there has a
	// value the hook can be sure of.
	late := false
	for s := scope; ; {
		// A class body's names are not visible to the methods inside it.
		if bs := a.bindings[pyBindingKey{name, s}]; len(bs) > 0 && (s == scope || !a.classes[s]) {
			switch {
			case late:
				if len(bs) == 1 {
					return bs[0].lit
				}
				return ""
			case a.compr[s]:
				return bs[len(bs)-1].lit
			}
			if i := sort.Search(len(bs), func(i int) bool { return bs[i].pos >= pos }); i > 0 {
				return bs[i-1].lit
			}
			// A name a function binds is local to all of it: read before
			// that binding it raises, rather than reading the outer name.
			if s != a.root && !a.classes[s] {
				return ""
			}
		}
		parent, ok := a.scopeOf[s]
		if !ok || s == a.root {
			return ""
		}
		if !a.compr[s] {
			late = true
		}
		s = parent
	}
}

// callName is the dotted name a call targets (`os.path.join`, `open`), and
// for a method call the method and the object it is called on.
func (a *pyAnalysis) callName(fn *sitter.Node) (name, method string, object *sitter.Node) {
	if fn == nil {
		return "", "", nil
	}
	switch fn.Type() {
	case "identifier":
		return a.text(fn), "", nil
	case "attribute":
		object = fn.ChildByFieldName("object")
		method = a.text(fn.ChildByFieldName("attribute"))
		if dotted := a.dotted(object); dotted != "" {
			return dotted + "." + method, method, object
		}
		return "", method, object
	}
	return "", "", nil
}

// dotted returns a short identifier chain (`os.path`) or "". A chain longer
// than any module path is no module path, and building it would cost time
// quadratic in its length.
func (a *pyAnalysis) dotted(n *sitter.Node) string {
	var parts []string
	for len(parts) < 32 && n != nil {
		switch n.Type() {
		case "identifier":
			parts = append(parts, a.text(n))
			for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
				parts[l], parts[r] = parts[r], parts[l]
			}
			return strings.Join(parts, ".")
		case "attribute":
			parts = append(parts, a.text(n.ChildByFieldName("attribute")))
			n = n.ChildByFieldName("object")
		default:
			return ""
		}
	}
	return ""
}

// args returns a call's positional arguments and its keyword arguments by
// name.
func (a *pyAnalysis) args(call *sitter.Node) ([]*sitter.Node, map[string]*sitter.Node) {
	var pos []*sitter.Node
	kw := map[string]*sitter.Node{}
	list := call.ChildByFieldName("arguments")
	if list == nil || list.Type() != "argument_list" {
		return nil, kw
	}
	for c := range list.NamedChildren() {
		switch c.Type() {
		case "keyword_argument":
			if name := c.ChildByFieldName("name"); name != nil {
				kw[a.text(name)] = c.ChildByFieldName("value")
			}
		case "comment":
		default:
			pos = append(pos, c)
		}
	}
	return pos, kw
}

// stringLiteral returns the value of a string with no interpolation.
func (a *pyAnalysis) stringLiteral(n *sitter.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	switch n.Type() {
	case "string":
		var b strings.Builder
		for c := range n.NamedChildren() {
			switch c.Type() {
			case "string_content", "escape_sequence":
				b.WriteString(a.text(c))
			case "interpolation":
				return "", false
			}
		}
		return b.String(), true
	case "concatenated_string":
		var b strings.Builder
		for c := range n.NamedChildren() {
			s, ok := a.stringLiteral(c)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		}
		return b.String(), true
	}
	return "", false
}

// eval returns the literal path an expression holds, or "" when it holds no
// literal the hook can name.
func (a *pyAnalysis) eval(n *sitter.Node, scope uintptr, depth int) string {
	if n == nil || !a.step(depth) || depth > pyMaxDepth {
		return ""
	}
	switch n.Type() {
	case "string", "concatenated_string":
		s, _ := a.stringLiteral(n)
		return s
	case "identifier":
		if a.reboundLater(a.text(n), n.StartByte(), scope) {
			return ""
		}
		return a.lookup(a.text(n), n.StartByte(), scope)
	case "parenthesized_expression":
		return a.eval(n.NamedChild(0), scope, depth+1)
	case "conditional_expression":
		if l := a.eval(n.NamedChild(0), scope, depth+1); l == a.eval(n.NamedChild(2), scope, depth+1) {
			return l
		}
	case "binary_operator":
		op := n.ChildByFieldName("operator")
		left := a.eval(n.ChildByFieldName("left"), scope, depth+1)
		right := a.eval(n.ChildByFieldName("right"), scope, depth+1)
		if op != nil && left != "" && right != "" && len(left)+len(right) < pyMaxLiteral {
			switch a.text(op) {
			case "+":
				return left + right
			case "/":
				if strings.HasPrefix(right, "/") {
					return right
				}
				return left + "/" + right
			}
		}
	case "call":
		name, _, _ := a.callName(n.ChildByFieldName("function"))
		if !pyPathCalls[name] {
			return ""
		}
		pos, _ := a.args(n)
		parts := make([]string, 0, len(pos))
		size := 0
		for _, p := range pos {
			l := a.eval(p, scope, depth+1)
			size += len(l) + 1
			if l == "" || size > pyMaxLiteral {
				return ""
			}
			if strings.HasPrefix(l, "/") {
				parts = parts[:0]
			}
			parts = append(parts, l)
		}
		return strings.Join(parts, "/")
	}
	return ""
}

// collect finds every file access and resolves its path against the
// bindings recorded by bind.
func (a *pyAnalysis) collect(n *sitter.Node, scope uintptr, depth int) {
	if !a.step(depth) {
		return
	}
	loop := a.loopEnd
	defer func() { a.loopEnd = loop }()
	// A function or lambda body runs later, outside the loop; a class body
	// or a comprehension runs where it is written.
	switch t := n.Type(); {
	case t == "function_definition" || t == "lambda":
		a.loopEnd = 0
	case (t == "for_statement" || t == "while_statement") && a.loopEnd == 0:
		a.loopEnd = n.EndByte()
	}
	inner := scope
	if pyScopeTypes[n.Type()] {
		inner = n.Id()
	}
	if n.Type() == "call" {
		a.collectCall(n, scope)
	}
	outer := a.outerParts(n)
	for c := range n.NamedChildren() {
		if c.Type() == "for_in_clause" && outer[c.Id()] {
			// The first iterable runs in the enclosing scope; the
			// clause's target and conditions belong to the comprehension.
			for cc := range c.NamedChildren() {
				s := inner
				if right := c.ChildByFieldName("right"); right != nil && cc.Id() == right.Id() {
					s = scope
				}
				a.collect(cc, s, depth+2)
			}
			continue
		}
		s := inner
		if outer[c.Id()] {
			s = scope
		}
		a.collect(c, s, depth+1)
	}
}

// outerParts are the children of a scope-opening node that Python
// evaluates in the enclosing scope, where the node is written: parameter
// defaults and annotations, a class's bases, and a comprehension's first
// iterable (marked by its for clause).
func (a *pyAnalysis) outerParts(n *sitter.Node) map[uintptr]bool {
	if !pyScopeTypes[n.Type()] {
		return nil
	}
	outer := map[uintptr]bool{}
	for _, field := range []string{"parameters", "return_type", "superclasses"} {
		if c := n.ChildByFieldName(field); c != nil {
			outer[c.Id()] = true
		}
	}
	if pyComprehensionTypes[n.Type()] {
		for c := range n.NamedChildren() {
			if c.Type() == "for_in_clause" {
				outer[c.Id()] = true
				break
			}
		}
	}
	return outer
}

func (a *pyAnalysis) collectCall(n *sitter.Node, scope uintptr) {
	name, method, object := a.callName(n.ChildByFieldName("function"))
	pos, kw := a.args(n)
	// mode is the open() mode, and false when it is not a literal: an
	// unknown mode could be a write, so the call gets no opinion.
	mode := func(at int) (string, bool) {
		m, ok := "", true
		if len(pos) > at {
			m, ok = a.stringLiteral(pos[at])
		}
		if v, has := kw["mode"]; has {
			m, ok = a.stringLiteral(v)
		}
		return m, ok
	}
	switch {
	case a.extra && (name == "os.open" || pyCommandCalls[name]) && !a.realModule(strings.SplitN(name, ".", 2)[0]):
		// A name the script binds to something other than the module.
	case a.extra && name == "os.open" && (kw["dir_fd"] != nil || hasSplat(pos)):
		// Relative to a directory the hook cannot see, or flags it cannot.
	case a.extra && name == "os.open":
		path := kw["path"]
		if len(pos) > 0 {
			path = pos[0]
		}
		flags := kw["flags"]
		if len(pos) > 1 {
			flags = pos[1]
		}
		// A read is a read; a write is only a write target, answered after
		// the rest of the command: it never makes a read an edit.
		if write, ok := a.osOpenWrites(flags); ok && path != nil {
			switch lit := a.eval(path, scope, 0); {
			case !write:
				if lit != "" && looksLikeSourceFile(lit) && !strings.ContainsAny(lit, "*?$") && !slices.Contains(a.facts.osReads, lit) && len(a.facts.osReads) < bashWriteProbeLimit {
					a.facts.osReads = append(a.facts.osReads, lit)
				}
			case lit != "" && looksLikeSourceFile(lit) && !strings.ContainsAny(lit, "*?$") && len(a.facts.writes) < bashWriteProbeLimit:
				a.facts.writes = append(a.facts.writes, lit)
			}
		}
	case a.extra && pyCommandCalls[name]:
		if line, ok := a.commandLine(name, pos, kw); ok && len(a.facts.commands) < maxNestedCommands {
			a.facts.commands = append(a.facts.commands, line)
		}
	case name == "open" && a.shadowed("open"):
		// The script defines or imports its own open.
	case name == "open" || (object != nil && method == "open" && pyOpenModules[a.dotted(object)]):
		path := kw["file"]
		if len(pos) > 0 {
			path = pos[0]
		}
		if m, ok := mode(1); ok && path != nil && !a.discardedParseCheck(n) {
			a.access(a.eval(path, scope, 0), m)
		}
	case object != nil && method == "open" && name != "os.open":
		// Path.open — only on an object that is a literal path, so
		// tarfile.open / zipfile.open and the like are not mistaken for it.
		if m, ok := mode(0); ok {
			if lit := a.eval(object, scope, 0); lit != "" {
				a.access(lit, m)
			}
		}
	case object != nil && (method == "read_text" || method == "read_bytes"):
		a.access(a.eval(object, scope, 0), "")
	case object != nil && (method == "write_text" || method == "write_bytes"):
		a.access(a.eval(object, scope, 0), "w")
	}
}

// osOpenWrites reads the flags of os.open built from os.O_* names joined by
// `|`: write is true when they open the file for writing (O_WRONLY, O_RDWR,
// O_TRUNC, O_APPEND), false for a read — O_CREAT alone changes no file that
// exists. ok is false when the flags are anything else, since a value the
// hook cannot read could be a write, and for O_PATH or O_DIRECTORY, which
// read no file content.
func (a *pyAnalysis) osOpenWrites(flags *sitter.Node) (write, ok bool) {
	names := map[string]bool{}
	var walk func(n *sitter.Node, depth int) bool
	walk = func(n *sitter.Node, depth int) bool {
		if n == nil || depth > pyMaxDepth || !a.step(depth) {
			return false
		}
		var flag string
		switch n.Type() {
		case "binary_operator":
			op := n.ChildByFieldName("operator")
			return op != nil && a.text(op) == "|" &&
				walk(n.ChildByFieldName("left"), depth+1) && walk(n.ChildByFieldName("right"), depth+1)
		case "parenthesized_expression":
			return walk(n.NamedChild(0), depth+1)
		case "attribute":
			var ok bool
			if flag, ok = strings.CutPrefix(a.dotted(n), "os."); !ok || !a.realModule("os") {
				return false
			}
		case "identifier":
			// Imported from os once and never rebound.
			flag = a.text(n)
			imported := a.osImports[flag] && a.bindings[pyBindingKey{flag, a.root}] != nil && a.bindingCount(flag) == 1
			if !imported && (!a.osStar || a.shadowed(flag)) {
				return false
			}
		default:
			return false
		}
		if !strings.HasPrefix(flag, "O_") || strings.Contains(flag, ".") {
			return false
		}
		names[flag] = true
		return true
	}
	if !walk(flags, 0) || names["O_PATH"] || names["O_DIRECTORY"] {
		return false, false
	}
	return names["O_WRONLY"] || names["O_RDWR"] || names["O_TRUNC"] || names["O_APPEND"], true
}

// commandLine returns the command line a pyCommandCalls call runs when it
// is literal: a string the shell runs as it is, or a list or tuple of
// strings joined with shell quoting. false for anything else, and for a
// call that runs it in another directory (cwd=) or with another program
// (executable=). os.system, os.popen and getoutput always run a string
// through the shell; the Popen family runs a string through it only with
// shell=True (without, the string is a program name), and a list only
// without it (with, the items past the first are arguments to the shell).
func (a *pyAnalysis) commandLine(name string, pos []*sitter.Node, kw map[string]*sitter.Node) (string, bool) {
	// *args and **kwargs may carry anything, cwd= and shell= included.
	if len(pos) == 0 || kw["cwd"] != nil || kw["executable"] != nil || hasSplat(pos) {
		return "", false
	}
	alwaysShell := name == "os.system" || name == "os.popen" || strings.HasPrefix(name, "subprocess.get")
	shell, hasShell := kw["shell"]
	// An escape in the literal reads differently to Python and the shell.
	literal := func(n *sitter.Node) (string, bool) {
		s, ok := a.stringLiteral(n)
		return s, ok && s != "" && !strings.Contains(s, `\`)
	}
	switch arg := pos[0]; arg.Type() {
	case "string", "concatenated_string":
		if !alwaysShell && (!hasShell || shell.Type() != "true") {
			return "", false
		}
		return literal(arg)
	case "list", "tuple":
		if alwaysShell || hasShell && shell.Type() != "false" {
			return "", false
		}
		var words []string
		for c := range arg.NamedChildren() {
			w, ok := literal(c)
			if !ok || len(words) >= 256 {
				return "", false
			}
			words = append(words, shellQuoteWord(w))
		}
		return strings.Join(words, " "), len(words) > 0
	}
	return "", false
}

// recordImport counts the names an import binds for realModule: a plain
// `import os` / `import os.path` binds the module itself, and the module
// of a `from module import …` is recorded as bound too. Bare os.O_* flag
// names imported from os are recorded for os.open.
func (a *pyAnalysis) recordImport(n *sitter.Node) {
	if n.Type() == "import_statement" {
		for c := range n.NamedChildren() {
			if c.Type() == "dotted_name" && c.NamedChildCount() > 0 {
				a.imports[a.text(c.NamedChild(0))]++
			}
		}
		return
	}
	module := n.ChildByFieldName("module_name")
	if module == nil {
		return
	}
	a.countFromModule(module)
	fromOS := a.text(module) == "os"
	for c := range n.NamedChildren() {
		switch {
		case c.Id() == module.Id():
		case fromOS && c.Type() == "wildcard_import":
			a.osStar = true
		case fromOS && c.Type() == "dotted_name":
			a.osImports[a.text(c)] = true
		}
	}
}

// countFromModule counts the names bind records for the module of a
// `from module import …`, a dotted name alone or inside a relative import.
func (a *pyAnalysis) countFromModule(n *sitter.Node) {
	for c := range n.NamedChildren() {
		switch c.Type() {
		case "identifier":
			if n.Type() == "dotted_name" {
				a.fromNames[a.text(c)]++
			}
		case "dotted_name":
			a.countFromModule(c)
		}
	}
}

// hasSplat reports whether a call's arguments include *args or **kwargs.
func hasSplat(args []*sitter.Node) bool {
	for _, arg := range args {
		if t := arg.Type(); t == "list_splat" || t == "dictionary_splat" {
			return true
		}
	}
	return false
}

// realModule reports whether name is bound only by plain `import name`
// statements, so name.attr is the module's own: never aliased onto
// another module, assigned, defined, deleted or declared global.
func (a *pyAnalysis) realModule(name string) bool {
	return a.imports[name] > 0 && !a.ambiguous[name] && a.bindingCount(name) == a.imports[name]+a.fromNames[name]
}

// bindingCount returns how many times the script binds name, in any scope.
func (a *pyAnalysis) bindingCount(name string) int {
	if a.bindCount == nil {
		a.bindCount = make(map[string]int, len(a.bindings))
		for key, bs := range a.bindings {
			a.bindCount[key.name] += len(bs)
		}
	}
	return a.bindCount[name]
}

// reboundLater reports whether the name read at pos is bound again further
// down a loop that runs the read: on the next pass it holds that value.
func (a *pyAnalysis) reboundLater(name string, pos uint32, scope uintptr) bool {
	if a.loopEnd == 0 {
		return false
	}
	for s := scope; ; s = a.scopeOf[s] {
		bs := a.bindings[pyBindingKey{name, s}]
		if i := sort.Search(len(bs), func(i int) bool { return bs[i].pos > pos }); i < len(bs) && bs[i].pos <= a.loopEnd {
			return true
		}
		// Out through the comprehensions and class bodies the loop holds.
		if s == a.root || (!a.compr[s] && !a.classes[s]) {
			return false
		}
	}
}

// shadowed reports whether the script binds name itself anywhere.
func (a *pyAnalysis) shadowed(name string) bool {
	if a.bound == nil {
		a.bound = make(map[string]bool, len(a.bindings))
		for key := range a.bindings {
			a.bound[key.name] = true
		}
	}
	return a.bound[name]
}

// access records one read or write of a path ("" when unnamed).
func (a *pyAnalysis) access(lit, mode string) {
	if strings.ContainsAny(mode, "wax+") {
		if lit != "" {
			a.facts.writeBack[lit] = true
		} else {
			a.unresolved = true
		}
	}
	if strings.ContainsAny(mode, "wax") || lit == "" {
		return // a pure write, or a read of a path the hook cannot name
	}
	if !a.facts.seen[lit] && looksLikeSourceFile(lit) && !strings.ContainsAny(lit, "*?$") {
		a.facts.seen[lit] = true
		a.facts.reads = append(a.facts.reads, lit)
	}
}

// discardedParseCheck reports whether an open() call is read only to be
// syntax-checked and thrown away: `ast.parse(open(p).read())` or
// `compile(open(p).read(), …)` standing as a statement of its own.
func (a *pyAnalysis) discardedParseCheck(open *sitter.Node) bool {
	attr := open.Parent()
	if attr == nil || attr.Type() != "attribute" || a.text(attr.ChildByFieldName("attribute")) != "read" {
		return false
	}
	readCall := attr.Parent()
	if readCall == nil || readCall.Type() != "call" {
		return false
	}
	argList := readCall.Parent()
	if argList == nil || argList.Type() != "argument_list" {
		return false
	}
	check := argList.Parent()
	if check == nil || check.Type() != "call" {
		return false
	}
	name, _, _ := a.callName(check.ChildByFieldName("function"))
	if name != "ast.parse" && name != "compile" && name != "parse" {
		return false
	}
	stmt := check.Parent()
	return stmt != nil && stmt.Type() == "expression_statement"
}
