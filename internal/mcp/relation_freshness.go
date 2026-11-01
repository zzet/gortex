package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// relationFreshnessWait bounds how long a change tool waits for the edits in
// its relation scope before it answers with a label instead.
const relationFreshnessWait = 250 * time.Millisecond

// maxLabelledPendingFiles caps the pending_files list of a label;
// pending_file_count always carries the full count.
const maxLabelledPendingFiles = 16

// relationFreshness labels an answer read from graph relations while edits in
// its scope are still being published. The zero value is a fresh scope.
type relationFreshness struct {
	scope  string
	state  string   // "", "pending" or "failed"
	files  []string // graph-keyed paths of the lagging in-scope edits, sorted
	waited time.Duration
}

// relationWaitDeadline is when a change tool stops waiting for its relation
// scope: relationFreshnessWait from now, or an earlier wait_deadline. With
// require_fresh it is the request's own freshness deadline instead. The
// middleware has already refused a malformed or past wait_deadline, so one
// the handler cannot take passed after admission and ends the wait now.
func relationWaitDeadline(ctx context.Context, freshness requestFreshness) time.Time {
	now := time.Now()
	if freshness.err != nil {
		return now
	}
	if freshness.requireFresh {
		return freshness.effectiveDeadline(now, ctx)
	}
	deadline := now.Add(relationFreshnessWait)
	if freshness.hasDeadline && freshness.deadline.Before(deadline) {
		return freshness.deadline
	}
	return deadline
}

// awaitRelationFreshness waits until deadline for the edits in the relation
// scope of ids and files, then labels whatever in that scope is still pending
// or failed. A deadline already passed labels without waiting. Edits outside
// the scope are neither waited on nor reported.
func (s *Server) awaitRelationFreshness(ctx context.Context, ids, files []string, deadline time.Time) relationFreshness {
	started := time.Now()
	label := s.freshnessFor(ctx, "relations", ids, files)
	if label.state == "" || label.state == "failed" || !deadline.After(started) {
		label.waited = time.Since(started)
		return label
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case <-ticker.C:
			label = s.freshnessFor(ctx, "relations", ids, files)
			if label.state != "" && label.state != "failed" {
				continue
			}
		case <-timer.C:
			label = s.freshnessFor(ctx, "relations", ids, files)
		case <-ctx.Done():
		}
		label.waited = time.Since(started)
		return label
	}
}

// laggingMutationReceipts returns the request's mutation receipts whose edit
// the graph has not published: still pending, or terminally failed.
func (s *Server) laggingMutationReceipts(ctx context.Context) []*mutationReceipt {
	var lagging []*mutationReceipt
	if paths, watched := s.canonicalTierPaths(ctx); watched {
		for path, tiers := range paths {
			if !tiers.Failed && !tiers.RelationsAt.IsZero() {
				continue
			}
			receipt := &mutationReceipt{path: path, generation: tiers.Generation}
			if tiers.Failed {
				receipt.completed = true
				receipt.result.Err = errors.New("file publication failed")
			}
			lagging = append(lagging, receipt)
		}
		return lagging
	}
	for _, receipt := range s.mutationReceiptsForRepos(ctx) {
		if outcome := receipt.outcome(true); outcome.Pending || outcome.Err != nil || !outcome.Reindexed {
			lagging = append(lagging, receipt)
		}
	}
	return lagging
}

// relationScope reports whether an edit to a file can change the relations
// of ids: the file defines one of them, holds an edge into one of them, is one
// of files (graph-keyed), or its current bytes name one of them as a whole
// identifier, which is how a call added by an edit the graph has not read yet
// shows up. An id that does not resolve contributes the file and name its
// spelling carries, so it narrows the scope instead of widening it.
func (s *Server) relationScope(ctx context.Context, ids, files []string) func(absPath string) bool {
	scoped := make(map[string]bool, len(files)+len(ids))
	for _, file := range files {
		scoped[s.graphRelPath(ctx, file)] = true
	}
	var names []string
	reader := s.readerFor(ctx)
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		node := reader.GetNode(id)
		if node == nil {
			if file := graph.IDFile(id); file != "" {
				scoped[file] = true
			}
			names = append(names, identifierName(id[strings.LastIndex(id, ":")+1:]))
			continue
		}
		scoped[node.FilePath] = true
		names = append(names, identifierName(node.Name))
		for _, edge := range reader.GetInEdges(id) {
			file := edge.FilePath
			if file == "" {
				file = graph.IDFile(edge.From)
			}
			if file != "" {
				scoped[file] = true
			}
		}
	}
	named := make(map[string]bool)
	return func(absPath string) bool {
		if scoped[s.repoRelative(absPath)] {
			return true
		}
		hit, seen := named[absPath]
		if !seen && len(names) > 0 {
			data, err := os.ReadFile(absPath)
			hit = err == nil && namesAnyIdentifier(data, names)
			named[absPath] = hit
		}
		return hit
	}
}

// identifierName is the last identifier of a possibly qualified name, the
// spelling a call site uses: Recv.Method is called as Method.
func identifierName(name string) string {
	return name[strings.LastIndex(name, ".")+1:]
}

// namesAnyIdentifier reports whether data holds one of names as a whole
// identifier rather than as part of a longer one.
func namesAnyIdentifier(data []byte, names []string) bool {
	for _, name := range names {
		if name == "" {
			continue
		}
		needle := []byte(name)
		for offset := 0; offset < len(data); {
			i := bytes.Index(data[offset:], needle)
			if i < 0 {
				break
			}
			start, end := offset+i, offset+i+len(needle)
			if (start == 0 || !isIdentByte(data[start-1])) && (end == len(data) || !isIdentByte(data[end])) {
				return true
			}
			offset = start + 1
		}
	}
	return false
}

// line is the relations line a text answer leads with.
func (f relationFreshness) line() string {
	scope := f.scope
	if scope == "" {
		scope = "relations"
	}
	return fmt.Sprintf("%s: %s (%d files)", scope, f.state, len(f.files))
}

// requireFreshError is the answer to a require_fresh call whose wait ended
// with relations in its scope still lagging.
func (f relationFreshness) requireFreshError() *mcp.CallToolResult {
	message := fmt.Sprintf("require_fresh: %s after waiting %d ms: %s",
		f.line(), f.waited.Milliseconds(), strings.Join(f.files[:min(maxLabelledPendingFiles, len(f.files))], ", "))
	if f.state == "failed" {
		message += "; " + mutationFreshnessRecoveryHint()
	}
	return mcp.NewToolResultError(message)
}

// merge folds the label of another scope the same answer read into f: the
// files of both, failed over pending, and the time spent waiting on each.
func (f relationFreshness) merge(other relationFreshness) relationFreshness {
	if f.scope == "" {
		f.scope = other.scope
	}
	f.waited += other.waited
	if other.state == "" {
		return f
	}
	if f.state != "failed" {
		f.state = other.state
	}
	seen := make(map[string]bool, len(f.files))
	for _, file := range f.files {
		seen[file] = true
	}
	for _, file := range other.files {
		if !seen[file] {
			f.files = append(f.files, file)
		}
	}
	sort.Strings(f.files)
	return f
}

// labelRelations keeps the pre-walk gaps as evidence even if they completed
// during the read, and also includes edits admitted while the walk ran.
func (s *Server) labelRelations(ctx context.Context, gaps []*mutationReceipt, ids, files []string) relationFreshness {
	return s.freshnessFor(ctx, "relations", ids, files, gaps)
}

// labelled is payload with the label's fields merged in, a struct payload
// being re-read as its JSON object first. A fresh scope returns payload
// unchanged.
func (f relationFreshness) labelled(payload any) any {
	if f.state == "" {
		return payload
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return payload
	}
	var fields map[string]any
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return payload
	}
	f.apply(fields)
	return fields
}

// provisionalSite is a line of a file whose edit the graph has not published
// that names a symbol as a whole identifier: a reference the graph cannot show
// yet, reported so a mutator neither misses it nor rewrites it. Line is 0 for
// the symbol's own file, which is provisional as a whole.
type provisionalSite struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// provisionalSites scans the files of gaps for lines naming node, leaving out
// the lines where g already holds a reference to it. When the node's own file
// is among them it is one site without a line: until that edit publishes, the
// graph's span of the node may not hold, and a line that names the node there
// may be a new caller or the definition itself.
func (s *Server) provisionalSites(g graph.Reader, gaps []*mutationReceipt, node *graph.Node) []provisionalSite {
	names := []string{identifierName(node.Name)}
	known := make(map[string]bool)
	for _, e := range g.GetInEdges(node.ID) {
		if caller := g.GetNode(e.From); caller != nil && isReferencingEdgeKind(e.Kind) {
			known[fmt.Sprintf("%s:%d", caller.FilePath, e.Line)] = true
		}
	}
	seen := make(map[string]bool)
	var sites []provisionalSite
	for _, receipt := range gaps {
		file := s.repoRelative(receipt.path)
		if seen[file] {
			continue
		}
		seen[file] = true
		if file == node.FilePath {
			sites = append(sites, provisionalSite{File: file})
			continue
		}
		data, err := os.ReadFile(receipt.path)
		if err != nil || !namesAnyIdentifier(data, names) {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if namesAnyIdentifier([]byte(line), names) && !known[fmt.Sprintf("%s:%d", file, i+1)] {
				sites = append(sites, provisionalSite{File: file, Line: i + 1, Text: strings.TrimSpace(line)})
			}
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].File != sites[j].File {
			return sites[i].File < sites[j].File
		}
		return sites[i].Line < sites[j].Line
	})
	return sites
}
