package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
)

// Reading files under a view of committed state.
//
// A checkout's view has a working copy behind it, so a file read resolves an
// absolute path and reads the disk. A ref view has none: the content it serves
// exists only as git objects, and the canonical checkout on disk holds some
// other branch. Resolving a path against that checkout would return bytes from
// the wrong state of the world and look exactly like a correct read, so the
// path resolution is replaced rather than supplemented — repo-relative reads
// go through the tree the view is pinned to, and a file location is reported
// as a gortex-view:// identity rather than as a path that names nothing.

// refViewWithdrawBudget bounds the one write a file read can make — the
// withdrawal of the source capability when the object store no longer holds
// the view's blobs. The record is best effort and the read's answer does not
// depend on it, so waiting for a saturated writer buys nothing.
const refViewWithdrawBudget = 2 * time.Second

// refViewFiles serves file bytes out of one view's committed tree. It is built
// per request and closed with it: the tree source spawns a git child on the
// first read and holds it for every read after that.
type refViewFiles struct {
	// store records the withdrawal a pruned object causes. Nil disables it —
	// the read still fails, it just leaves no trace on the generation.
	store *store_sqlite.Store

	fingerprint  string
	repoPrefix   string
	repoDir      string
	treeOID      string
	generationID int64

	mu      sync.Mutex
	tree    *source.GitTreeSource
	opened  bool
	openErr error
	closed  bool
}

// available reports whether this surface can produce bytes: a tree to read,
// and a repository to read it out of.
//
// It is NOT the test for "does this request read a committed tree" — that is
// viewReadsCommittedTree, and the two were the same predicate until a surface
// that could not open its tree meant the request fell back to a working copy
// instead of being told. See refViewFilesFor.
func (f *refViewFiles) available() bool {
	return f != nil && f.repoDir != "" && f.treeOID != ""
}

// sourceUnavailable is what a committed-tree view answers when it holds no
// tree to read bytes out of.
//
// It is a refusal rather than a miss, and it is the same code and capability
// name the capability evaluation would have refused a caller that required
// source.snapshot with: the alternative is to resolve the path against a
// working copy, which returns real bytes of some other state of the world and
// looks exactly like a correct read.
func (f *refViewFiles) sourceUnavailable() error {
	missing := "the repository its objects live in is not reachable"
	switch {
	case f == nil:
		missing = "no file surface is bound to it"
	case f.treeOID == "" && f.repoDir == "":
		missing = "neither the tree it reads nor the repository holding it is known"
	case f.treeOID == "":
		missing = "no committed tree is bound to it"
	}
	return graphview.NewViewError(graphview.CodeCapabilityUnavailable, fmt.Sprintf(
		"this request reads a committed tree and cannot serve %s: %s",
		graphview.CapSourceSnapshot, missing))
}

// uri renders the identity of one file inside this view.
func (f *refViewFiles) uri(relPath string) string {
	return graphview.ViewFileURI(f.fingerprint, f.repoPrefix, relPath)
}

// graphPath renders a tree-relative path the way the graph spells it, so a
// file read through the view keys the same node lookups a disk read does.
func (f *refViewFiles) graphPath(relPath string) string {
	if f.repoPrefix == "" {
		return relPath
	}
	return path.Join(f.repoPrefix, relPath)
}

// read returns the bytes of one repo-relative path in the view's tree.
//
// A path the tree does not carry is a plain miss. A path it carries whose blob
// the local object store no longer holds is source_object_missing, and it
// withdraws the view's source capability on the way out: the generation can
// still answer graph and search questions from rows it already holds, but it
// can no longer produce bytes, and a caller that requires them must be told
// before it asks again.
func (f *refViewFiles) read(ctx context.Context, relPath string) ([]byte, error) {
	if !f.available() {
		return nil, f.sourceUnavailable()
	}
	tree, err := f.open(ctx)
	if err != nil {
		return nil, err
	}
	reader, _, err := tree.Open(relPath)
	if err != nil {
		if errors.Is(err, source.ErrObjectMissing) {
			f.withdraw()
			return nil, graphview.WrapViewError(graphview.CodeSourceObjectMissing,
				fmt.Sprintf("%s is gone from the local object store", f.uri(relPath)), err)
		}
		return nil, fmt.Errorf("could not read %s: %w", f.uri(relPath), err)
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

// open builds the tree source on first use. A construction failure is cached:
// it is a property of the tree, not of the path, so retrying it once per file
// read would cost a git process per miss.
func (f *refViewFiles) open(ctx context.Context) (*source.GitTreeSource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, errors.New("the view's file source is closed")
	}
	if !f.available() {
		// No git child is ever spawned for a surface with nothing to open, so
		// a view that carries one holds no process to release.
		return nil, f.sourceUnavailable()
	}
	if !f.opened {
		f.opened = true
		f.tree, f.openErr = source.NewGitTreeSource(ctx, f.repoDir, f.treeOID)
		if f.openErr != nil && errors.Is(f.openErr, source.ErrObjectMissing) {
			f.withdrawLocked()
			f.openErr = graphview.WrapViewError(graphview.CodeSourceObjectMissing,
				fmt.Sprintf("tree %s is gone from the local object store", f.treeOID), f.openErr)
		}
	}
	return f.tree, f.openErr
}

// close releases the git child the tree source holds.
func (f *refViewFiles) close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.tree != nil {
		_ = f.tree.Close()
		f.tree = nil
	}
}

// withdraw marks the view's source snapshot unavailable. It touches that one
// producer row and nothing else, so the graph and search capabilities the
// generation already populated keep answering.
func (f *refViewFiles) withdraw() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withdrawLocked()
}

func (f *refViewFiles) withdrawLocked() {
	if f.store == nil || f.generationID <= 0 {
		return
	}
	// Bounded, because this is a write on a read path: the store's mutation
	// gate is held for as long as a build's transactions run, and a read that
	// queued there would turn a missing blob into a request that never
	// answers. The next read of the same absent object withdraws again.
	ctx, cancel := context.WithTimeout(context.Background(), refViewWithdrawBudget)
	defer cancel()
	// A repeat withdrawal reports a stale guard, which is the right answer and
	// nothing to act on: the capability is already gone.
	_ = f.store.Catalog().WithdrawProducer(ctx, f.generationID,
		string(graphview.CapSourceSnapshot),
		"the object store no longer holds this view's blobs")
}

// relPath normalises a caller's path onto the tree's namespace.
//
// The tree is one repository's content, so its paths carry no repo prefix; a
// caller that spells one (the form every graph node id uses) has it stripped.
// An absolute path is refused outright rather than stripped down to something
// that happens to match: it names a location in a working copy, and this view
// has none.
func (f *refViewFiles) relPath(raw string) (string, error) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return "", errors.New("path is empty")
	}
	cleaned = strings.ReplaceAll(cleaned, "\\", "/")
	if strings.HasPrefix(cleaned, "/") || strings.Contains(cleaned, ":\\") ||
		(len(cleaned) > 1 && cleaned[1] == ':') {
		return "", fmt.Errorf(
			"%q is an absolute path, and this request reads a committed tree that is not checked out anywhere; "+
				"name the file relative to the repository", raw)
	}
	if f.repoPrefix != "" {
		cleaned = strings.TrimPrefix(cleaned, f.repoPrefix+"/")
	}
	cleaned = path.Clean(cleaned)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%q leaves the repository", raw)
	}
	return cleaned, nil
}

// refViewFilesFor returns the committed-tree file surface this request reads
// through, nil when the request reads a working copy as it always has.
//
// Every view that reads a committed tree gets a surface, including one that
// cannot produce bytes. Returning nil for those is what put the byte lane and
// the text lane on different snapshots: nil sends the caller down the
// working-copy resolvers (tools_fileops.go: resolveFilePath has no
// committed-tree guard of its own, unlike resolveNodePath:487 and
// resolveGraphPath:556), and the bytes that come back are the canonical
// checkout's — a different state of the world, returned under a rider that
// says the request was served exactly the ref it asked for. A surface that
// refuses says so instead; see sourceUnavailable.
//
// A request that reads a working copy is also where the working copy's own
// coherence is checked: this is the one place on the byte lane that runs
// exactly once per answer, so the route the view pinned is revalidated here
// rather than once per resolved path.
func refViewFilesFor(ctx context.Context) *refViewFiles {
	view := requestViewFromContext(ctx)
	if !viewReadsCommittedTree(view) {
		noteWorktreeRouteDrift(ctx, view, graphview.CapSourceSnapshot)
		return nil
	}
	if view.files != nil {
		return view.files
	}
	return committedTreeWithoutASource(view)
}

// committedTreeWithoutASource is the surface a committed-tree view that was
// never given one reads through: a routed checkout whose working-tree layer
// the route withdrew after the request bound it (see viewReadsCommittedTree).
//
// It carries the view's identity so a refusal names the right view, and no
// repository or tree, so every read refuses rather than opening anything. That
// is deliberately the whole of it: minting a real tree source here would need
// the top layer's TreeOID off the catalog and would spawn a git child on a
// value no request lifecycle closes, because the view was built without a file
// surface for close() to release (view_request.go: close releases v.files).
// Serving those bytes belongs where the view is constructed; refusing to serve
// the wrong ones belongs here.
func committedTreeWithoutASource(view *requestView) *refViewFiles {
	surface := &refViewFiles{}
	if view.materialized != nil {
		surface.fingerprint = view.materialized.ID.Fingerprint()
		surface.repoPrefix = view.materialized.ID.RepoPrefix
	}
	return surface
}

// readViewFile resolves a caller's path against the pinned tree and returns
// the bytes plus the repo-relative path they came from.
func readViewFile(ctx context.Context, files *refViewFiles, raw string) ([]byte, string, error) {
	rel, err := files.relPath(raw)
	if err != nil {
		return nil, "", err
	}
	content, err := files.read(ctx, rel)
	if err != nil {
		return nil, rel, err
	}
	return content, rel, nil
}
