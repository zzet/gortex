// Package parityresiduals compares a composed view with a clean whole index,
// row by row and grouped by kind. It excuses a difference only when an entry
// of a residual list matches it. The list is a Markdown file whose table rows
// start with "| R". The parity tests and the end-of-run check of the private
// daemon harness both read the same file, so the list and the check cannot
// drift apart.
//
// Row renders (the kind-parity tests' identity rules):
//
//	node: id|file|name|start-end
//	edge: from>to|file:line   (from is "*" for an owner edge into a contract
//	                           whose owner edges two or more files record)
//
// Entry columns: id | class | kind | side | pattern | partner | the rest free
// text. class is node, edge or meta. side is view (a row only the view holds)
// or clean (a row only the clean index holds). pattern is a Go regular
// expression over the differing row. partner is "-" or a regular expression
// that some row of the same kind on the OTHER side must match, whole, with
// every named group the two expressions share captured equal. An entry with a
// partner therefore never excuses an unpaired row. A meta entry's pattern is
// matched against "row key=value" and is reported only; metadata never fails
// a comparison.
package parityresiduals

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Entry is one residual of the list.
type Entry struct {
	ID      string
	Class   string
	Kind    string
	Side    string
	Pattern *regexp.Regexp
	Partner *regexp.Regexp
}

// Load parses the residual table of a Markdown file.
func Load(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "| R") {
			continue
		}
		cells := splitRow(line)
		if len(cells) < 6 {
			return nil, fmt.Errorf("residual row %q: want at least 6 cells", line)
		}
		e := Entry{ID: cells[0], Class: cells[1], Kind: cells[2], Side: cells[3]}
		switch e.Class {
		case "node", "edge", "meta", "side":
		default:
			return nil, fmt.Errorf("%s: class %q", e.ID, e.Class)
		}
		if e.Side != "view" && e.Side != "clean" && e.Side != "-" {
			return nil, fmt.Errorf("%s: side %q", e.ID, e.Side)
		}
		if e.Pattern, err = regexp.Compile(unquote(cells[4])); err != nil {
			return nil, fmt.Errorf("%s: pattern: %w", e.ID, err)
		}
		if p := unquote(cells[5]); p != "-" && p != "" {
			if e.Partner, err = regexp.Compile(p); err != nil {
				return nil, fmt.Errorf("%s: partner: %w", e.ID, err)
			}
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no residual rows", path)
	}
	return out, nil
}

// splitRow splits a Markdown table row, honouring "\|" inside a cell.
func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) && line[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if line[i] == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(line[i])
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

func unquote(cell string) string {
	return strings.TrimSuffix(strings.TrimPrefix(cell, "`"), "`")
}

// Rows is one reader's rows under the identity rules, grouped by kind.
type Rows struct {
	Nodes, Edges map[string][]string
	// Meta is row render -> rendered metadata, for nodes and edges alike.
	Meta map[string]string
}

// Collect renders the rows of the given graph paths: every node GetFileNodes
// returns for them, every edge into or out of those nodes that is recorded at
// one of them, and, when the reader keeps a recorded-edge index
// (graph.RecordedEdgesOf), every other edge recorded at them.
func Collect(r graph.Reader, paths []string) Rows {
	out := Rows{Nodes: map[string][]string{}, Edges: map[string][]string{}, Meta: map[string]string{}}
	in := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		in[p] = struct{}{}
	}
	var ids []string
	kindOf := map[string]graph.NodeKind{}
	for _, p := range paths {
		for _, n := range r.GetFileNodes(p) {
			if n == nil {
				continue
			}
			ids = append(ids, n.ID)
			kindOf[n.ID] = n.Kind
			row := fmt.Sprintf("%s|%s|%s|%d-%d", n.ID, n.FilePath, n.Name, n.StartLine, n.EndLine)
			out.Nodes[string(n.Kind)] = append(out.Nodes[string(n.Kind)], row)
			out.Meta[row] = renderMeta(n.Meta)
		}
	}
	seen := map[string]struct{}{}
	var edges []*graph.Edge
	add := func(list []*graph.Edge) {
		for _, e := range list {
			if e == nil {
				continue
			}
			if _, ok := in[e.FilePath]; !ok {
				continue
			}
			key := fmt.Sprintf("%s>%s|%s|%s:%d", e.From, e.To, e.Kind, e.FilePath, e.Line)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			edges = append(edges, e)
		}
	}
	for _, list := range r.GetOutEdgesByNodeIDs(ids) {
		add(list)
	}
	for _, id := range ids {
		add(r.GetInEdges(id))
	}
	// Edges recorded at the paths whose endpoints both lie outside them
	// (an argument of a local variable into an unresolved callee:
	// `repo/unresolved::n>unresolved::uint64`) are reached only through the
	// reader's recorded-edge index; a reader without one leaves them out.
	if recorded, ok := graph.RecordedEdgesOf(r); ok {
		add(recorded.RecordedEdgesAt(paths))
	}
	ownerFiles := map[string]map[string]struct{}{}
	for _, e := range edges {
		if ownerEdge(e.Kind) {
			if ownerFiles[e.To] == nil {
				ownerFiles[e.To] = map[string]struct{}{}
			}
			ownerFiles[e.To][e.FilePath] = struct{}{}
		}
	}
	for _, e := range edges {
		from := e.From
		if ownerEdge(e.Kind) && len(ownerFiles[e.To]) > 1 {
			from = "*"
		}
		row := fmt.Sprintf("%s>%s|%s:%d", from, e.To, e.FilePath, e.Line)
		out.Edges[string(e.Kind)] = append(out.Edges[string(e.Kind)], row)
		out.Meta[string(e.Kind)+" "+row] = renderMeta(e.Meta)
	}
	for _, m := range []map[string][]string{out.Nodes, out.Edges} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	return out
}

func ownerEdge(k graph.EdgeKind) bool {
	return k == graph.EdgeProvides || k == graph.EdgeConsumes || k == graph.EdgeHandlesRoute
}

func renderMeta(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, ",")
}

// Difference is one row only one side holds.
type Difference struct {
	Class, Kind, Side, Row string
	// Residual is the id of the entry that excuses it, "" when none does.
	Residual string
}

// Result is a comparison's outcome.
type Result struct {
	Differences []Difference
	// Met counts the differences each residual excused, by id.
	Met map[string]int
	// MetaDiffers is "class kind row key" lines whose metadata differs,
	// with the residual that names them (or "" when none does); reported
	// only.
	MetaDiffers []string
}

// Unexcused is the differences no residual excuses.
func (r Result) Unexcused() []Difference {
	var out []Difference
	for _, d := range r.Differences {
		if d.Residual == "" {
			out = append(out, d)
		}
	}
	return out
}

// Compare compares view with clean, kind by kind, and names the residual that
// excuses each difference.
func Compare(view, clean Rows, entries []Entry) Result {
	return CompareWithProgress(view, clean, entries, nil)
}

// progressEvery is how many differences pass between two progress lines.
const progressEvery = 10000

// CompareWithProgress is Compare, telling progress (when not nil) the
// differences' counts by class, kind and side before any is excused, then a
// line every progressEvery differences with the time spent, and a last line.
// A comparison stopped part way thus still leaves its counts.
//
// A partner is looked up by key: for each partnered entry and kind, the other
// side's rows are matched against the partner once and indexed by the values
// of the named groups the pattern and the partner share (partners). The cost
// is one pass over each side's rows per partnered entry, not one per
// difference.
func CompareWithProgress(view, clean Rows, entries []Entry, progress func(string)) Result {
	started := time.Now()
	res := Result{Met: map[string]int{}}
	type pending struct {
		d     Difference
		other []string
	}
	var todo []pending
	for _, class := range []string{"node", "edge"} {
		v, c := view.Nodes, clean.Nodes
		if class == "edge" {
			v, c = view.Edges, clean.Edges
		}
		kinds := map[string]struct{}{}
		for k := range v {
			kinds[k] = struct{}{}
		}
		for k := range c {
			kinds[k] = struct{}{}
		}
		sorted := make([]string, 0, len(kinds))
		for k := range kinds {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, kind := range sorted {
			onlyView, onlyClean := diff(v[kind], c[kind])
			for _, row := range onlyView {
				todo = append(todo, pending{Difference{Class: class, Kind: kind, Side: "view", Row: row}, c[kind]})
			}
			for _, row := range onlyClean {
				todo = append(todo, pending{Difference{Class: class, Kind: kind, Side: "clean", Row: row}, v[kind]})
			}
			// Metadata of rows both sides hold.
			shared := map[string]int{}
			for _, row := range c[kind] {
				shared[row]++
			}
			for _, row := range v[kind] {
				if shared[row] == 0 {
					continue
				}
				key := row
				if class == "edge" {
					key = kind + " " + row
				}
				if view.Meta[key] == clean.Meta[key] {
					continue
				}
				line := fmt.Sprintf("%s %s %s view={%s} clean={%s}", class, kind, row, view.Meta[key], clean.Meta[key])
				id := ""
				for _, e := range entries {
					if e.Class == "meta" && e.Kind == kind && e.Pattern.MatchString(line) {
						id = e.ID
						break
					}
				}
				if id != "" {
					res.Met[id]++
				}
				res.MetaDiffers = append(res.MetaDiffers, fmt.Sprintf("[%s] %s", id, line))
			}
		}
	}
	if progress != nil {
		counts := map[string]int{}
		for _, p := range todo {
			counts[p.d.Class+" "+p.d.Kind+" only-"+p.d.Side]++
		}
		groups := make([]string, 0, len(counts))
		for g := range counts {
			groups = append(groups, g)
		}
		sort.Strings(groups)
		var b strings.Builder
		fmt.Fprintf(&b, "differences before excusing: %d (collected and diffed in %s)", len(todo), time.Since(started).Round(time.Millisecond))
		for _, g := range groups {
			fmt.Fprintf(&b, "\n  %s: %d", g, counts[g])
		}
		progress(b.String())
	}
	index := partners{}
	excusing := time.Now()
	for i, p := range todo {
		p.d.Residual = index.excuse(entries, p.d, p.other)
		res.Differences = append(res.Differences, p.d)
		if progress != nil && (i+1)%progressEvery == 0 {
			progress(fmt.Sprintf("excused %d of %d differences in %s", i+1, len(todo), time.Since(excusing).Round(time.Millisecond)))
		}
	}
	for _, d := range res.Differences {
		if d.Residual != "" {
			res.Met[d.Residual]++
		}
	}
	if progress != nil {
		progress(fmt.Sprintf("excused %d differences in %s (%d unexcused); comparison %s",
			len(todo), time.Since(excusing).Round(time.Millisecond), len(res.Unexcused()), time.Since(started).Round(time.Millisecond)))
	}
	return res
}

// partners indexes, per partnered entry and kind, the keys of the other
// side's rows that match the entry's partner: the values of the named groups
// the pattern and the partner share, in the partner's group order. It is
// built on first use and serves every later difference of that entry and kind.
type partners map[string]map[string]struct{}

// excuse names the first entry that excuses d, "" when none does.
func (p partners) excuse(entries []Entry, d Difference, other []string) string {
	for _, e := range entries {
		if e.Class != d.Class || e.Kind != d.Kind || e.Side != d.Side {
			continue
		}
		m := e.Pattern.FindStringSubmatch(d.Row)
		if m == nil {
			continue
		}
		if e.Partner == nil || p.has(e, d, m, other) {
			return e.ID
		}
	}
	return ""
}

// sharedGroups is the partner's group indexes whose names the pattern also
// has, with the pattern's index for each, in the partner's order.
func sharedGroups(e Entry) (partnerIdx, patternIdx []int) {
	pattern := map[string]int{}
	for i, name := range e.Pattern.SubexpNames() {
		if name != "" {
			if _, dup := pattern[name]; !dup {
				pattern[name] = i
			}
		}
	}
	seen := map[string]bool{}
	for i, name := range e.Partner.SubexpNames() {
		if j, ok := pattern[name]; ok && name != "" && !seen[name] {
			seen[name] = true
			partnerIdx, patternIdx = append(partnerIdx, i), append(patternIdx, j)
		}
	}
	return partnerIdx, patternIdx
}

func groupKey(m []string, idx []int) string {
	var b strings.Builder
	for _, i := range idx {
		b.WriteString(m[i])
		b.WriteByte(0)
	}
	return b.String()
}

// has reports whether some row of other matches e.Partner with every named
// group it shares with e.Pattern captured equal to m's.
func (p partners) has(e Entry, d Difference, m []string, other []string) bool {
	partnerIdx, patternIdx := sharedGroups(e)
	cacheKey := e.ID + "\x00" + d.Class + "\x00" + d.Kind + "\x00" + d.Side
	keys, built := p[cacheKey]
	if !built {
		keys = map[string]struct{}{}
		for _, row := range other {
			if pm := e.Partner.FindStringSubmatch(row); pm != nil {
				keys[groupKey(pm, partnerIdx)] = struct{}{}
			}
		}
		p[cacheKey] = keys
	}
	_, ok := keys[groupKey(m, patternIdx)]
	return ok
}

func diff(got, want []string) (onlyGot, onlyWant []string) {
	count := map[string]int{}
	for _, w := range want {
		count[w]++
	}
	for _, g := range got {
		if count[g] > 0 {
			count[g]--
			continue
		}
		onlyGot = append(onlyGot, g)
	}
	for w, c := range count {
		for ; c > 0; c-- {
			onlyWant = append(onlyWant, w)
		}
	}
	sort.Strings(onlyWant)
	return onlyGot, onlyWant
}

// Report renders a result: the residuals met with counts, the metadata
// differences, and every unexcused difference.
func Report(r Result) string {
	var b strings.Builder
	ids := make([]string, 0, len(r.Met))
	for id := range r.Met {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintf(&b, "residuals met:")
	if len(ids) == 0 {
		fmt.Fprintf(&b, " none")
	}
	for _, id := range ids {
		fmt.Fprintf(&b, " %s=%d", id, r.Met[id])
	}
	b.WriteByte('\n')
	for _, line := range r.MetaDiffers {
		fmt.Fprintf(&b, "  META %s\n", line)
	}
	for _, d := range r.Differences {
		tag := "UNEXCUSED"
		if d.Residual != "" {
			tag = d.Residual
		}
		fmt.Fprintf(&b, "  %s %s %s only %s: %s\n", tag, d.Class, d.Kind, d.Side, d.Row)
	}
	return b.String()
}

// ReportBounded renders a result for a log: the residuals met with counts,
// the metadata differences counted by class and kind, and the differences
// grouped by (residual or UNEXCUSED, class, kind, side) with their counts and
// at most perGroup rows each. Side is which reader holds the row (view or
// clean), so an unexcused row can be taken to the primary per-save path.
func ReportBounded(r Result, perGroup int) string {
	var b strings.Builder
	ids := make([]string, 0, len(r.Met))
	for id := range r.Met {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintf(&b, "residuals met:")
	if len(ids) == 0 {
		fmt.Fprintf(&b, " none")
	}
	for _, id := range ids {
		fmt.Fprintf(&b, " %s=%d", id, r.Met[id])
	}
	b.WriteByte('\n')
	metaGroups := map[string]int{}
	for _, line := range r.MetaDiffers {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			metaGroups[fields[0]+" "+fields[1]+" "+fields[2]]++
		}
	}
	metaKeys := make([]string, 0, len(metaGroups))
	for k := range metaGroups {
		metaKeys = append(metaKeys, k)
	}
	sort.Strings(metaKeys)
	for _, k := range metaKeys {
		fmt.Fprintf(&b, "  META %s: %d rows\n", k, metaGroups[k])
	}
	type group struct {
		key  string
		rows []string
	}
	groups := map[string]*group{}
	var order []string
	for _, d := range r.Differences {
		tag := "UNEXCUSED"
		if d.Residual != "" {
			tag = d.Residual
		}
		key := fmt.Sprintf("%s %s %s only-%s", tag, d.Class, d.Kind, d.Side)
		g := groups[key]
		if g == nil {
			g = &group{key: key}
			groups[key] = g
			order = append(order, key)
		}
		g.rows = append(g.rows, d.Row)
	}
	sort.Strings(order)
	unexcused := 0
	for _, key := range order {
		g := groups[key]
		if strings.HasPrefix(key, "UNEXCUSED") {
			unexcused += len(g.rows)
		}
		fmt.Fprintf(&b, "  %s: %d rows\n", key, len(g.rows))
		for i, row := range g.rows {
			if i == perGroup {
				fmt.Fprintf(&b, "      ... %d more\n", len(g.rows)-perGroup)
				break
			}
			fmt.Fprintf(&b, "      %s\n", row)
		}
	}
	fmt.Fprintf(&b, "unexcused rows: %d\n", unexcused)
	return b.String()
}

// WriteFull writes every difference and metadata line to path, stopping at
// limit bytes with a final line saying how much was left out.
func WriteFull(path string, r Result, limit int) error {
	var b strings.Builder
	written := 0
	left := 0
	add := func(line string) {
		if written+len(line)+1 > limit {
			left++
			return
		}
		b.WriteString(line)
		b.WriteByte('\n')
		written += len(line) + 1
	}
	for _, d := range r.Differences {
		tag := "UNEXCUSED"
		if d.Residual != "" {
			tag = d.Residual
		}
		add(fmt.Sprintf("%s %s %s only-%s: %s", tag, d.Class, d.Kind, d.Side, d.Row))
	}
	for _, line := range r.MetaDiffers {
		add("META " + line)
	}
	if left > 0 {
		fmt.Fprintf(&b, "... %d lines left out at the %d-byte limit\n", left, limit)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
