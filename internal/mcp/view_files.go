package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

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

const refViewSourceWithdrawalReason = "the object store no longer holds this view's blobs"

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

// available reports whether this request can read file content at all.
func (f *refViewFiles) available() bool {
	return f != nil && f.repoDir != "" && f.treeOID != ""
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
	// This is deliberately scheduling-only: a missing Git object is discovered
	// on a read path, and that answer must never queue behind SQLite's writer.
	// The exact producer row is withdrawn asynchronously; structural producers
	// remain untouched and duplicate misses coalesce in the store-lifetime
	// manager.
	_ = f.store.ScheduleProducerWithdrawal(
		f.generationID,
		string(graphview.CapSourceSnapshot),
		refViewSourceWithdrawalReason,
	)
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
func refViewFilesFor(ctx context.Context) *refViewFiles {
	view := requestViewFromContext(ctx)
	if view == nil || !view.files.available() {
		return nil
	}
	return view.files
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
