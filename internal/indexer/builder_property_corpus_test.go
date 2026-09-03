package indexer

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// The synthetic corpus the equivalence properties are generated over.
//
// A generated repository is a go module with two packages and a fixed
// scaffolding — the module file, the root package's types and the sub
// package — plus a variable number of root-package files the mutation
// scripts add to, edit, rename and remove.
//
// Two shapes are deliberately kept OUT of the corpus, because each one makes
// the differential oracle unable to answer rather than testing the builder:
//
//   - No builtin or stdlib types, and no imports outside the module. Both
//     make the resolver materialise repo-scoped stubs, whose ids carry no
//     path. A file-granular mask cannot reach them, which is the known
//     composition gap TestSparseGenerationClaimsPathlessIdentities pins by
//     name: GetRepoNodes and NodeCount answer from the layer's file list and
//     a pathless identity appears in no file list. Every signature here is
//     therefore typed by a repository-local type and every import names a
//     package inside the module.
//   - No callee is ever passed the same argument twice. An edge is keyed by
//     (from, to, kind), so `f(o, o)` collapses two arg_of edges that differ
//     only in arg_position onto one row, and which of the two survives is not
//     a property of the builder.
const (
	propModule    = "fixture"
	propSubImport = "fixture/sub"
)

// propScaffolding is the part of every generated repository a mutation script
// never touches. It carries the types every generated signature is written in,
// the interface the generated implementors satisfy, and the second package the
// import operation toggles a dependency on.
func propScaffolding() map[string]string {
	return map[string]string{
		"go.mod": "module " + propModule + "\n\ngo 1.22\n",
		"types.go": `package fixture

type Options struct{}

type Result struct{}

type Service interface {
	Do(o Options) Result
}
`,
		"sub/sub.go": `package sub

type Payload struct{}

func Assist() Payload {
	return Payload{}
}
`,
	}
}

// propFunc is the single exported function a generated file defines.
type propFunc struct {
	name   string
	params int    // 1 or 2 — the arity a signature change flips
	callee string // the name of a function in another file, or ""
	tail   bool   // return callee(p0) rather than callee(p0) as a statement
	rev    int    // body-only revision, rendered as a comment
}

// propFile is one generated root-package file.
type propFile struct {
	path      string
	recvType  string // non-empty: the file also defines a Service implementor
	importSub bool   // the file imports and calls the sub package
	fn        propFunc
}

func (f propFile) render() string {
	var b strings.Builder
	b.WriteString("package fixture\n\n")
	if f.importSub {
		fmt.Fprintf(&b, "import %q\n\n", propSubImport)
	}
	if f.recvType != "" {
		fmt.Fprintf(&b, "type %s struct{}\n\n", f.recvType)
		fmt.Fprintf(&b, "func (r %s) Do(o Options) Result {\n\treturn Result{}\n}\n\n", f.recvType)
	}
	params := make([]string, 0, f.fn.params)
	for i := 0; i < f.fn.params; i++ {
		params = append(params, fmt.Sprintf("p%d Options", i))
	}
	fmt.Fprintf(&b, "func %s(%s) Result {\n", f.fn.name, strings.Join(params, ", "))
	fmt.Fprintf(&b, "\t// revision %d\n", f.fn.rev)
	if f.importSub {
		b.WriteString("\tsub.Assist()\n")
	}
	switch {
	case f.fn.callee != "" && f.fn.tail:
		fmt.Fprintf(&b, "\treturn %s(p0)\n", f.fn.callee)
	case f.fn.callee != "":
		fmt.Fprintf(&b, "\t%s(p0)\n\treturn Result{}\n", f.fn.callee)
	default:
		b.WriteString("\treturn Result{}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// propCorpus is the mutable half of a generated repository.
type propCorpus struct {
	files []propFile
	next  int // the counter every generated name is minted from
}

// tree renders the whole repository — scaffolding included — as the path to
// content map the fixture writers take.
func (c *propCorpus) tree() map[string]string {
	tree := propScaffolding()
	for _, f := range c.files {
		tree[f.path] = f.render()
	}
	return tree
}

// callersOf lists the indices of files whose function calls name.
func (c *propCorpus) callersOf(name string) []int {
	var out []int
	for i := range c.files {
		if c.files[i].fn.callee == name {
			out = append(out, i)
		}
	}
	return out
}

// referenced lists the indices of files whose function some other file calls —
// the deletion operation's candidates, because removing one of them leaves a
// dangling reference the closure has to re-resolve.
func (c *propCorpus) referenced() []int {
	var out []int
	for i := range c.files {
		if len(c.callersOf(c.files[i].fn.name)) > 0 {
			out = append(out, i)
		}
	}
	return out
}

// --- the generator ------------------------------------------------------

type propOpKind string

const (
	propOpAddFile         propOpKind = "add_file"
	propOpChangeSignature propOpKind = "change_signature"
	propOpBodyOnly        propOpKind = "body_only"
	propOpDeleteFile      propOpKind = "delete_file"
	propOpRenameFile      propOpKind = "rename_file"
	propOpRenameSymbol    propOpKind = "rename_symbol"
	propOpChangeImports   propOpKind = "change_imports"
)

var propOpKinds = []propOpKind{
	propOpAddFile,
	propOpChangeSignature,
	propOpBodyOnly,
	propOpDeleteFile,
	propOpRenameFile,
	propOpRenameSymbol,
	propOpChangeImports,
}

// propScript is one generated mutation of a corpus: the operations that were
// applied, in order, and the path renames they performed. Renames are carried
// separately because they are the one operation the disk applier cannot infer
// from a tree difference — git has to be told, or a staged rename never
// appears in a dirty sample.
type propScript struct {
	Ops     []string
	Renames [][2]string
}

func (s propScript) String() string {
	if len(s.Ops) == 0 {
		return "<empty>"
	}
	return strings.Join(s.Ops, " -> ")
}

// propMinFiles is the floor the deletion and rename operations respect, so a
// script can never shrink a corpus to the point where later operations have
// nothing to work on.
const propMinFiles = 3

// propInitialCorpus builds the starting repository: four to eight generated
// files on top of the three scaffolding ones, with cross-file calls forming a
// chain and two Service implementors.
func propInitialCorpus(rng *rand.Rand) *propCorpus {
	c := &propCorpus{}
	n := 4 + rng.Intn(5)
	for i := 0; i < n; i++ {
		f := propFile{
			path: fmt.Sprintf("f%d.go", i),
			fn: propFunc{
				name:   fmt.Sprintf("Fn%d", i),
				params: 1 + rng.Intn(2),
				tail:   rng.Intn(2) == 0,
			},
		}
		// A call into one of the files already minted, so the corpus has a
		// cross-file reference graph before any script runs.
		if i > 0 && rng.Intn(4) != 0 {
			f.fn.callee = c.files[rng.Intn(i)].fn.name
		}
		if i < 2 {
			f.recvType = fmt.Sprintf("Impl%d", i)
		}
		if rng.Intn(3) == 0 {
			f.importSub = true
		}
		c.files = append(c.files, f)
	}
	c.next = n
	return c
}

// propGenerateScript applies two to six operations to c in place and returns
// what it did. Operations that cannot apply to the corpus as it stands are
// re-drawn, so a script always carries the number of operations it reports.
func propGenerateScript(rng *rand.Rand, c *propCorpus) propScript {
	var script propScript
	want := 2 + rng.Intn(5)
	// Paths that were present when the script started and have not been
	// touched by it yet: the only safe rename sources, because git can only
	// move a path that is tracked and still where it was.
	untouched := make(map[string]bool, len(c.files))
	for _, f := range c.files {
		untouched[f.path] = true
	}
	for len(script.Ops) < want {
		var applied bool
		for attempt := 0; attempt < len(propOpKinds)*2 && !applied; attempt++ {
			kind := propOpKinds[rng.Intn(len(propOpKinds))]
			desc, rename, ok := propApplyOp(rng, c, kind, untouched)
			if !ok {
				continue
			}
			script.Ops = append(script.Ops, desc)
			if rename != [2]string{} {
				script.Renames = append(script.Renames, rename)
			}
			applied = true
		}
		if !applied {
			break
		}
	}
	return script
}

// propApplyOp performs one operation. It reports the operation in a form that
// replays by eye, the path rename it performed if any, and whether the corpus
// admitted it at all.
func propApplyOp(
	rng *rand.Rand,
	c *propCorpus,
	kind propOpKind,
	untouched map[string]bool,
) (string, [2]string, bool) {
	var none [2]string
	switch kind {
	case propOpAddFile:
		if len(c.files) == 0 {
			return "", none, false
		}
		target := c.files[rng.Intn(len(c.files))]
		f := propFile{
			path: fmt.Sprintf("g%d.go", c.next),
			fn: propFunc{
				name:   fmt.Sprintf("Gen%d", c.next),
				params: 1 + rng.Intn(2),
				callee: target.fn.name,
				tail:   rng.Intn(2) == 0,
			},
		}
		if rng.Intn(3) == 0 {
			f.importSub = true
		}
		c.next++
		c.files = append(c.files, f)
		delete(untouched, f.path)
		return fmt.Sprintf("add_file %s calling %s", f.path, f.fn.callee), none, true

	case propOpChangeSignature:
		// A signature change on a function somebody calls is what forces the
		// closure to re-derive a file the diff does not mention.
		candidates := c.referenced()
		if len(candidates) == 0 {
			candidates = propAllIndices(c)
		}
		if len(candidates) == 0 {
			return "", none, false
		}
		i := candidates[rng.Intn(len(candidates))]
		if c.files[i].fn.params == 1 {
			c.files[i].fn.params = 2
		} else {
			c.files[i].fn.params = 1
		}
		delete(untouched, c.files[i].path)
		return fmt.Sprintf("change_signature %s -> %s/%d params",
			c.files[i].path, c.files[i].fn.name, c.files[i].fn.params), none, true

	case propOpBodyOnly:
		if len(c.files) == 0 {
			return "", none, false
		}
		i := rng.Intn(len(c.files))
		c.files[i].fn.rev++
		delete(untouched, c.files[i].path)
		return fmt.Sprintf("body_only %s rev %d", c.files[i].path, c.files[i].fn.rev), none, true

	case propOpDeleteFile:
		if len(c.files) <= propMinFiles {
			return "", none, false
		}
		candidates := c.referenced()
		if len(candidates) == 0 {
			candidates = propAllIndices(c)
		}
		i := candidates[rng.Intn(len(candidates))]
		gone := c.files[i]
		c.files = append(c.files[:i:i], c.files[i+1:]...)
		delete(untouched, gone.path)
		return fmt.Sprintf("delete_file %s defining %s", gone.path, gone.fn.name), none, true

	case propOpRenameFile:
		if len(c.files) <= propMinFiles {
			return "", none, false
		}
		var candidates []int
		for i := range c.files {
			if untouched[c.files[i].path] {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) == 0 {
			return "", none, false
		}
		i := candidates[rng.Intn(len(candidates))]
		from := c.files[i].path
		to := fmt.Sprintf("moved%d.go", c.next)
		c.next++
		c.files[i].path = to
		delete(untouched, from)
		return fmt.Sprintf("rename_file %s -> %s", from, to), [2]string{from, to}, true

	case propOpRenameSymbol:
		// The definition and every call site move together, which is what a
		// refactor does and what makes the operation touch more than one file.
		candidates := c.referenced()
		if len(candidates) == 0 {
			return "", none, false
		}
		i := candidates[rng.Intn(len(candidates))]
		old := c.files[i].fn.name
		renamed := fmt.Sprintf("Ren%d", c.next)
		c.next++
		c.files[i].fn.name = renamed
		for _, j := range c.callersOf(old) {
			c.files[j].fn.callee = renamed
			delete(untouched, c.files[j].path)
		}
		delete(untouched, c.files[i].path)
		return fmt.Sprintf("rename_symbol %s -> %s", old, renamed), none, true

	case propOpChangeImports:
		if len(c.files) == 0 {
			return "", none, false
		}
		i := rng.Intn(len(c.files))
		c.files[i].importSub = !c.files[i].importSub
		delete(untouched, c.files[i].path)
		return fmt.Sprintf("change_imports %s sub=%v", c.files[i].path, c.files[i].importSub), none, true
	}
	return "", none, false
}

func propAllIndices(c *propCorpus) []int {
	out := make([]int, len(c.files))
	for i := range c.files {
		out[i] = i
	}
	return out
}

// propTreeDelta is the per-path difference between two rendered trees, in the
// vocabulary the disk applier and the staging picker both work in.
type propTreeDelta struct {
	Written []string
	Removed []string
}

func propDiffTrees(old, current map[string]string) propTreeDelta {
	var delta propTreeDelta
	for path, body := range current {
		if old[path] != body {
			delta.Written = append(delta.Written, path)
		}
	}
	for path := range old {
		if _, present := current[path]; !present {
			delta.Removed = append(delta.Removed, path)
		}
	}
	sort.Strings(delta.Written)
	sort.Strings(delta.Removed)
	return delta
}
