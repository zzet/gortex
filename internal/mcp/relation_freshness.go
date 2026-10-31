package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	wire "github.com/gortexhq/gcx-go"
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
	lagging := s.laggingMutationReceipts(ctx)
	if len(lagging) == 0 {
		return relationFreshness{waited: time.Since(started)}
	}
	inScope := s.relationScope(ctx, ids, files)
	if remaining := time.Until(deadline); remaining > 0 {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
	wait:
		for _, receipt := range lagging {
			if !inScope(receipt.path) {
				continue
			}
			select {
			case <-receipt.done:
			case <-timer.C:
				break wait
			case <-ctx.Done():
				break wait
			}
		}
		lagging = s.laggingMutationReceipts(ctx)
	}

	states := make(map[string]string)
	for _, receipt := range lagging {
		if !inScope(receipt.path) {
			continue
		}
		key := s.repoRelative(receipt.path)
		if receipt.outcome(true).Pending {
			states[key] = "pending"
		} else if states[key] == "" {
			states[key] = "failed"
		}
	}
	label := relationFreshness{waited: time.Since(started)}
	for path, state := range states {
		label.files = append(label.files, path)
		if label.state != "failed" {
			label.state = state
		}
	}
	sort.Strings(label.files)
	return label
}

// laggingMutationReceipts returns the request's mutation receipts whose edit
// the graph has not published: still pending, or terminally failed.
func (s *Server) laggingMutationReceipts(ctx context.Context) []*mutationReceipt {
	var lagging []*mutationReceipt
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
		scoped[file] = true
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

// apply writes the label into an answer's fields. A fresh scope writes
// nothing, so an answer with nothing lagging keeps its shape.
func (f relationFreshness) apply(result map[string]any) {
	if f.state == "" {
		return
	}
	result["complete"] = false
	result["graph_freshness"] = f.state
	result["freshness_scope"] = "relations"
	result["pending_file_count"] = len(f.files)
	result["pending_files"] = f.files[:min(maxLabelledPendingFiles, len(f.files))]
	if f.state == "failed" {
		result["recovery_hint"] = mutationFreshnessRecoveryHint()
	}
}

// line is the relations line a text answer leads with.
func (f relationFreshness) line() string {
	return fmt.Sprintf("relations: %s (%d files)", f.state, len(f.files))
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

// decorate makes a rendered text answer lead with its label: TOON and plain
// text start with the relations line; GCX keeps GCX1 as line 1, carries the
// label as header meta and the relations line as a comment on line 2. JSON
// already carries the label in its fields and passes through unchanged.
func (f relationFreshness) decorate(res *mcp.CallToolResult) *mcp.CallToolResult {
	if f.state == "" || res == nil || res.IsError || len(res.Content) == 0 {
		return res
	}
	text, ok := res.Content[0].(mcp.TextContent)
	if !ok || strings.HasPrefix(strings.TrimSpace(text.Text), "{") {
		return res
	}
	body := text.Text
	if strings.HasPrefix(body, wire.Tag) {
		body, _ = injectGCXHeaderMeta(body, map[string]any{
			"graph_freshness":    f.state,
			"freshness_scope":    "relations",
			"pending_file_count": len(f.files),
		})
		head, rows, _ := strings.Cut(body, wire.RowSep)
		body = head + wire.RowSep + wire.CommentPrefix + " " + f.line() + wire.RowSep + rows
	} else {
		body = f.line() + "\n" + body
	}
	return rebuildTextResult(res, body)
}
