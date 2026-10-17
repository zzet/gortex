package resolver

import (
	"sort"
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

// A per-save run of the fn-value gate over a changed file re-resolved every
// captured function value in it, and a value no same-file function binds is
// looked up by name across the whole store (resolveFnValueCrossModuleMemo):
// on a store of many repositories, names like Err, Done or C return thousands
// of nodes each, so a one-declaration edit of a file with a few dozen callback
// values cost seconds.
//
// The outcome of a value depends only on the functions and methods named what
// it names (and, for `<self>`, on its registering function). A save changes
// only the changed files' nodes, so a value whose name no changed file defines
// or stopped defining under another identity resolves exactly as it did: its
// prior registrations are republished at its current line. FnValuePrior
// carries what that needs, read from the changed files' prior rows before they
// were evicted.

// FnValuePrior is the fn-value state of the files one incremental mutation
// re-parses, read before the files are evicted.
type FnValuePrior struct {
	// Files are the graph paths it was captured for.
	Files map[string]struct{}
	// candidates is, per candidate key (fnValuePriorKey), the candidate's
	// resolution inputs; conflicting inputs under one key are
	// fnValueInputsConflict.
	candidates map[string]string
	// registrations is, per candidate key, the registrations it landed.
	registrations map[string][]*graph.Edge
	// callables is, per name, the function/method identities the files
	// defined.
	callables map[string]map[string]struct{}
	// groups is, per file, captured name and form (fnValueGroupKey), the
	// candidates' shared resolution inputs and the registrations they
	// landed, whoever registered them: what a candidate whose registering
	// function was renamed resolves to.
	groups map[string]*fnValueGroup
}

// fnValueGroup is the candidates of one file capturing one name in one form.
type fnValueGroup struct {
	inputs string
	// selfRisk is set when a registering function carried the captured
	// name itself: the gate never binds a value to its own registrar, so the
	// group's targets depend on who registered it.
	selfRisk bool
	regs     []*graph.Edge
}

func fnValueGroupKey(file, name string, edge *graph.Edge) string {
	form, _ := edge.Meta["fn_ref_form"].(string)
	return file + "\x00" + name + "\x00" + form
}

// fnValueRegistrarNamed reports whether a registering function's identity
// ends in name (the gate's self-exclusion can then apply to it).
func fnValueRegistrarNamed(from, name string) bool {
	last := from
	if i := strings.LastIndex(last, "::"); i >= 0 {
		last = last[i+2:]
	}
	if i := strings.LastIndex(last, "."); i >= 0 {
		last = last[i+1:]
	}
	return last == name
}

// NewFnValuePrior returns an empty prior for the files.
func NewFnValuePrior() *FnValuePrior {
	return &FnValuePrior{
		Files:         make(map[string]struct{}),
		candidates:    make(map[string]string),
		registrations: make(map[string][]*graph.Edge),
		callables:     make(map[string]map[string]struct{}),
		groups:        make(map[string]*fnValueGroup),
	}
}

func (p *FnValuePrior) group(key string) *fnValueGroup {
	if p.groups == nil {
		p.groups = make(map[string]*fnValueGroup)
	}
	g := p.groups[key]
	if g == nil {
		g = &fnValueGroup{inputs: "\x02unset"}
		p.groups[key] = g
	}
	return g
}

// AddFile records one file's prior nodes and each prior node's outgoing rows.
func (p *FnValuePrior) AddFile(graphPath string, nodes []*graph.Node, outByNode map[string][]*graph.Edge) {
	if p == nil {
		return
	}
	p.Files[graphPath] = struct{}{}
	for _, node := range nodes {
		if node == nil || node.ID == "" {
			continue
		}
		if node.Kind == graph.KindFunction || node.Kind == graph.KindMethod {
			ids := p.callables[node.Name]
			if ids == nil {
				ids = make(map[string]struct{})
				p.callables[node.Name] = ids
			}
			ids[node.ID] = struct{}{}
		}
		for _, edge := range outByNode[node.ID] {
			if edge == nil || edge.Meta == nil || edge.Kind != graph.EdgeReferences || edge.FilePath != graphPath {
				continue
			}
			via, _ := edge.Meta["via"].(string)
			name, _ := edge.Meta[metaFnValueName].(string)
			if name == "" {
				continue
			}
			key := fnValuePriorKey(edge.From, name, edge)
			switch via {
			case fnValueCandidateVia:
				inputs := fnValueCandidateInputs(edge)
				if old, seen := p.candidates[key]; seen && old != inputs {
					inputs = fnValueInputsConflict
				}
				p.candidates[key] = inputs
				g := p.group(fnValueGroupKey(graphPath, name, edge))
				switch g.inputs {
				case "\x02unset":
					g.inputs = fnValueCandidateInputs(edge)
				case fnValueCandidateInputs(edge):
				default:
					g.inputs = fnValueInputsConflict
				}
				if fnValueRegistrarNamed(edge.From, name) {
					g.selfRisk = true
				}
			case fnValueRegistrationVia:
				c := *edge
				p.registrations[key] = append(p.registrations[key], &c)
				g := p.group(fnValueGroupKey(graphPath, name, edge))
				dup := false
				for _, r := range g.regs {
					if r.To == c.To {
						dup = true
						break
					}
				}
				if !dup {
					g.regs = append(g.regs, &c)
				}
			}
		}
	}
}

// Merge adds other's files to p.
func (p *FnValuePrior) Merge(other *FnValuePrior) *FnValuePrior {
	if other == nil {
		return p
	}
	if p == nil {
		return other
	}
	for f := range other.Files {
		p.Files[f] = struct{}{}
	}
	for k, v := range other.candidates {
		if old, seen := p.candidates[k]; seen && old != v {
			v = fnValueInputsConflict
		}
		p.candidates[k] = v
	}
	for k, v := range other.registrations {
		p.registrations[k] = append(p.registrations[k], v...)
	}
	for k, g := range other.groups {
		dst := p.group(k)
		switch dst.inputs {
		case "\x02unset":
			dst.inputs = g.inputs
		case g.inputs:
		default:
			dst.inputs = fnValueInputsConflict
		}
		dst.selfRisk = dst.selfRisk || g.selfRisk
		dst.regs = append(dst.regs, g.regs...)
	}
	for name, ids := range other.callables {
		dst := p.callables[name]
		if dst == nil {
			dst = make(map[string]struct{})
			p.callables[name] = dst
		}
		for id := range ids {
			dst[id] = struct{}{}
		}
	}
	return p
}

// fnValueInputsConflict marks a key two prior candidates shared with different
// resolution inputs; no candidate's inputs equal it, so it is never reused.
const fnValueInputsConflict = "\x01conflict"

// fnValuePriorKey keys a candidate or registration by its registering
// function, the captured name and the captured form; the line is left out so
// a save that moves the registration keeps its key.
func fnValuePriorKey(from, name string, edge *graph.Edge) string {
	form, _ := edge.Meta["fn_ref_form"].(string)
	return from + "\x00" + name + "\x00" + form
}

// fnValueCandidateInputs is everything a candidate's resolution reads off the
// candidate itself.
func fnValueCandidateInputs(edge *graph.Edge) string {
	recvHint, _ := edge.Meta["fn_ref_recv_hint"].(string)
	ungated, _ := edge.Meta["fn_value_ungated"].(bool)
	skipGate, _ := edge.Meta["skip_gate"].(bool)
	var b strings.Builder
	b.WriteString(recvHint)
	if ungated {
		b.WriteString("\x00u")
	}
	if skipGate {
		b.WriteString("\x00s")
	}
	return b.String()
}

// changedNames is every name whose function/method identities in the prior's
// files differ between the prior and g now: the names whose resolution the
// save can have moved.
func (p *FnValuePrior) changedNames(g graph.Store) map[string]struct{} {
	now := make(map[string]map[string]struct{})
	files := make([]string, 0, len(p.Files))
	for f := range p.Files {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		for _, node := range g.GetFileNodes(f) {
			if node == nil || (node.Kind != graph.KindFunction && node.Kind != graph.KindMethod) {
				continue
			}
			ids := now[node.Name]
			if ids == nil {
				ids = make(map[string]struct{})
				now[node.Name] = ids
			}
			ids[node.ID] = struct{}{}
		}
	}
	changed := make(map[string]struct{})
	for name, before := range p.callables {
		if !sameIDSet(before, now[name]) {
			changed[name] = struct{}{}
		}
	}
	for name, after := range now {
		if !sameIDSet(p.callables[name], after) {
			changed[name] = struct{}{}
		}
	}
	return changed
}

func sameIDSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

// reusable returns the registrations a candidate landed before the save, and
// whether they still stand: the candidate is in a file the prior covers, it
// existed there with the same resolution inputs, and no changed file moved
// the identities of the name it captures.
func (p *FnValuePrior) reusable(edge *graph.Edge, name string, changed map[string]struct{}) ([]*graph.Edge, bool) {
	if p == nil {
		return nil, false
	}
	if _, covered := p.Files[edge.FilePath]; !covered {
		return nil, false
	}
	if _, moved := changed[name]; moved {
		return nil, false
	}
	key := fnValuePriorKey(edge.From, name, edge)
	inputs, existed := p.candidates[key]
	if existed {
		if inputs != fnValueCandidateInputs(edge) {
			return nil, false
		}
		return p.registrations[key], true
	}
	// The registering function is new (a rename of it, say): the value
	// resolves as the file's other captures of the same name and form did,
	// unless the capture is a self-member one (resolved against the
	// registrar's own type) or a registrar carries the captured name (the
	// gate's self-exclusion then depends on who registers it).
	g := p.groups[fnValueGroupKey(edge.FilePath, name, edge)]
	if g == nil || g.selfRisk || g.inputs != fnValueCandidateInputs(edge) || fnValueRegistrarNamed(edge.From, name) {
		return nil, false
	}
	if recvHint, _ := edge.Meta["fn_ref_recv_hint"].(string); recvHint == "<self>" {
		return nil, false
	}
	fnValueGroupReused.Add(1)
	return g.regs, true
}

// fnValueGroupReused counts the candidates reused through their file's
// same-name group rather than their own registrar's key (tests).
var fnValueGroupReused atomic.Int64

// republishFnValueRegistration is a prior registration landed again from the candidate at its
// current place.
func republishFnValueRegistration(prior, candidate *graph.Edge) *graph.Edge {
	meta := make(map[string]any, len(prior.Meta))
	for k, v := range prior.Meta {
		meta[k] = v
	}
	return &graph.Edge{
		From:            candidate.From,
		To:              prior.To,
		Kind:            graph.EdgeReferences,
		FilePath:        candidate.FilePath,
		Line:            candidate.Line,
		Confidence:      prior.Confidence,
		ConfidenceLabel: prior.ConfidenceLabel,
		Origin:          prior.Origin,
		Meta:            meta,
	}
}

// fnValueReused / fnValueResolved count the candidates republished from a
// prior and resolved (tests and the per-save log).
var fnValueReused, fnValueResolved atomic.Int64

// FnValueGateCounts returns how many fn-value candidates were republished
// from a prior and how many resolved, since the process started.
func FnValueGateCounts() (reused, resolved int64) {
	return fnValueReused.Load(), fnValueResolved.Load()
}

// ResolveFnValueCallbacksForFiles gates the fn-value candidates recorded in
// files, republishing from prior what the save cannot have moved. It is the
// per-save pass for a save that re-parsed the files without moving anything
// the framework pass re-derives (a body edit): the files' registrations were
// evicted with them and nothing else lands them again.
func ResolveFnValueCallbacksForFiles(g graph.Store, files []string, prior *FnValuePrior) int {
	if g == nil || len(files) == 0 {
		return 0
	}
	inFiles := make(map[string]struct{}, len(files))
	var ids []string
	for _, f := range files {
		inFiles[f] = struct{}{}
		for _, node := range g.GetFileNodes(f) {
			if node != nil && node.ID != "" {
				ids = append(ids, node.ID)
			}
		}
	}
	sort.Strings(ids)
	var candidates []*graph.Edge
	outByNode := g.GetOutEdgesByNodeIDs(ids)
	for _, id := range ids {
		for _, edge := range outByNode[id] {
			if edge == nil || edge.Meta == nil || edge.Kind != graph.EdgeReferences {
				continue
			}
			if _, ok := inFiles[edge.FilePath]; !ok {
				continue
			}
			if via, _ := edge.Meta["via"].(string); via != fnValueCandidateVia {
				continue
			}
			name, _ := edge.Meta[metaFnValueName].(string)
			if name == "" || isFnValueNonTarget(name) {
				continue
			}
			candidates = append(candidates, edge)
		}
	}
	return resolveFnValueCandidates(g, candidates, prior)
}
