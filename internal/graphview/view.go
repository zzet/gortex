// Package graphview holds the vocabulary for request-pinned graph views — the
// identity of a view, the capabilities a view can serve, the selector a caller
// uses to ask for one, the stable error codes a view request fails with, and
// the lease accounting that keeps a pinned generation alive while a request
// still reads it — together with the machinery that turns that vocabulary into
// a readable graph: a persisted generation read as an overlay layer, the
// stacking of such layers over a base corpus, and the routed materialization
// of one checkout's view.
//
// The vocabulary half is stdlib-only on purpose, and this file, capability.go,
// errors.go, selector.go, lease.go and rider.go carry all of it: nothing in
// them depends on more than the standard library and internal/graph. That is a
// property of the files, not of the import. Go links whole packages, so
// generation_layer.go, compose.go and materialize.go put the storage layer —
// store_sqlite and the CGo SQLite driver under it — into the binary of every
// graphview importer. A consumer that has to stay storage-free needs the
// vocabulary lifted into a package of its own; what the split buys today is
// that every layer speaking about views — the server, the query engine, the
// indexer — reads one definition of a view identity and a capability state and
// none of them can drift on it.
package graphview

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// LayerKind names the sort of layer stacked on top of a base graph.
type LayerKind string

const (
	// LayerCommit is a materialized commit layer: content that exists in git
	// history and has been indexed, so it is addressed by a generation.
	LayerCommit LayerKind = "commit"
	// LayerDirty is the working-tree layer of a checkout: uncommitted on-disk
	// edits. The daemon indexes them, so this too is generation-addressed.
	LayerDirty LayerKind = "dirty"
	// LayerBuffer is an editor-buffer layer: unsaved content pushed by a
	// client. It never gets a generation — it is addressed by a fingerprint
	// over the session and the buffer content.
	LayerBuffer LayerKind = "buffer"
)

// Valid reports whether k is one of the defined layer kinds.
func (k LayerKind) Valid() bool {
	switch k {
	case LayerCommit, LayerDirty, LayerBuffer:
		return true
	default:
		return false
	}
}

// LayerRef identifies one layer of a view.
//
// Commit and dirty layers carry a positive Generation and no
// BufferFingerprint; buffer layers are the mirror image — Generation is always
// 0 and BufferFingerprint is required. Validate enforces that split, because a
// buffer layer that claimed a generation would make two different contents
// share one view identity.
type LayerRef struct {
	Kind              LayerKind `json:"kind"`
	LayerID           string    `json:"layer_id"`
	Generation        int64     `json:"generation,omitempty"`
	BufferFingerprint string    `json:"buffer_fingerprint,omitempty"`
}

// Validate reports whether the layer is well-formed.
func (l LayerRef) Validate() error {
	if !l.Kind.Valid() {
		return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("unknown layer kind %q", string(l.Kind)))
	}
	if l.LayerID == "" {
		return NewViewError(CodeInvalidViewSelector, "layer id is required")
	}
	if l.Kind == LayerBuffer {
		if l.Generation != 0 {
			return NewViewError(CodeInvalidViewSelector, "buffer layer must not carry a generation")
		}
		if l.BufferFingerprint == "" {
			return NewViewError(CodeInvalidViewSelector, "buffer layer requires a buffer fingerprint")
		}
		return nil
	}
	if l.Generation <= 0 {
		return NewViewError(CodeInvalidViewSelector, fmt.Sprintf("%s layer requires a positive generation", string(l.Kind)))
	}
	if l.BufferFingerprint != "" {
		return NewViewError(CodeInvalidViewSelector, "only a buffer layer carries a buffer fingerprint")
	}
	return nil
}

// Equal reports whether two layer refs are the same layer.
func (l LayerRef) Equal(other LayerRef) bool { return l == other }

// RepoViewID identifies the exact graph content one repository contributes to
// a view: a base graph at a generation, plus the layers stacked on it ordered
// bottom to top. Layer order is part of the identity — the same layers applied
// in a different order are a different view.
type RepoViewID struct {
	RepoPrefix     string     `json:"repo_prefix"`
	BaseGraphID    string     `json:"base_graph_id"`
	BaseGeneration int64      `json:"base_generation"`
	Layers         []LayerRef `json:"layers,omitempty"`
}

// NewRepoViewID validates its arguments and returns the repo view identity.
// The layer slice is copied, so a later mutation by the caller cannot change
// an identity that has already been fingerprinted.
func NewRepoViewID(repoPrefix, baseGraphID string, baseGeneration int64, layers ...LayerRef) (RepoViewID, error) {
	v := RepoViewID{
		RepoPrefix:     repoPrefix,
		BaseGraphID:    baseGraphID,
		BaseGeneration: baseGeneration,
		Layers:         slices.Clone(layers),
	}
	if err := v.Validate(); err != nil {
		return RepoViewID{}, err
	}
	return v, nil
}

// Validate reports whether the repo view is well-formed.
func (v RepoViewID) Validate() error {
	if v.RepoPrefix == "" {
		return NewViewError(CodeInvalidViewSelector, "repo prefix is required")
	}
	if v.BaseGraphID == "" {
		return NewViewError(CodeInvalidViewSelector, "base graph id is required")
	}
	if v.BaseGeneration <= 0 {
		return NewViewError(CodeInvalidViewSelector, "base generation must be positive")
	}
	for i, l := range v.Layers {
		if err := l.Validate(); err != nil {
			return WrapViewError(CodeInvalidViewSelector, fmt.Sprintf("layer %d", i), err)
		}
	}
	return nil
}

// Equal reports whether two repo views name the same content.
func (v RepoViewID) Equal(other RepoViewID) bool {
	return v.RepoPrefix == other.RepoPrefix &&
		v.BaseGraphID == other.BaseGraphID &&
		v.BaseGeneration == other.BaseGeneration &&
		slices.Equal(v.Layers, other.Layers)
}

// Fingerprint returns the hex sha256 of the canonical encoding of v. Two repo
// views that differ in any field fingerprint differently, and the value is
// stable across processes and builds.
func (v RepoViewID) Fingerprint() string {
	return hashCanonical(v.canonical())
}

// WorkspaceViewID identifies the whole view a request reads: one repo view per
// repository in scope. The canonical order is by RepoPrefix; NewWorkspaceViewID
// establishes it and Fingerprint re-establishes it defensively, so a
// hand-assembled value in another order still fingerprints the same.
type WorkspaceViewID struct {
	Repos []RepoViewID `json:"repos"`
}

// NewWorkspaceViewID validates every repo view, rejects two views of the same
// repository, and returns the repos in canonical (RepoPrefix) order.
func NewWorkspaceViewID(repos ...RepoViewID) (WorkspaceViewID, error) {
	v := WorkspaceViewID{Repos: slices.Clone(repos)}
	slices.SortStableFunc(v.Repos, compareRepoViews)
	if err := v.Validate(); err != nil {
		return WorkspaceViewID{}, err
	}
	return v, nil
}

// Validate reports whether the workspace view is well-formed: every repo view
// valid, no repository named twice, and the repos in canonical order.
func (v WorkspaceViewID) Validate() error {
	for i, r := range v.Repos {
		if err := r.Validate(); err != nil {
			return WrapViewError(CodeInvalidViewSelector, fmt.Sprintf("repo %d", i), err)
		}
		if i == 0 {
			continue
		}
		switch prev := v.Repos[i-1].RepoPrefix; {
		case prev == r.RepoPrefix:
			return NewViewError(CodeSelectorConflict, fmt.Sprintf("repository %q appears twice in one view", r.RepoPrefix))
		case prev > r.RepoPrefix:
			return NewViewError(CodeInvalidViewSelector, "repo views must be ordered by repo prefix")
		}
	}
	return nil
}

// Equal reports whether two workspace views name the same content. It compares
// canonical encodings, so it ignores the order the repos happen to be stored
// in — the same rule Fingerprint uses.
func (v WorkspaceViewID) Equal(other WorkspaceViewID) bool {
	return bytes.Equal(v.canonical(), other.canonical())
}

// Fingerprint returns the hex sha256 of the canonical encoding of v.
func (v WorkspaceViewID) Fingerprint() string {
	return hashCanonical(v.canonical())
}

// compareRepoViews orders repo views by prefix, breaking ties on the canonical
// encoding so the sort is total even for a malformed value with a duplicated
// prefix (which Validate then rejects).
func compareRepoViews(a, b RepoViewID) int {
	if c := strings.Compare(a.RepoPrefix, b.RepoPrefix); c != 0 {
		return c
	}
	return bytes.Compare(a.canonical(), b.canonical())
}

// Canonical encoding.
//
// Fingerprints must be injective: no two distinct identities may encode to the
// same bytes. Joining fields with a separator cannot promise that, because a
// separator can appear inside a repo prefix or a graph id ("a" + "|" + "b|c"
// and "a|b" + "|" + "c" are the same string). So every string is written
// length-prefixed, every int64 as a fixed 8 bytes, every list as a count
// followed by its elements, and each struct behind a domain tag that keeps a
// repo encoding from ever being read as a workspace encoding. Field boundaries
// are therefore recoverable from the byte stream alone.
const (
	repoViewTag      = "gortex.graphview.repo.v1"
	workspaceViewTag = "gortex.graphview.workspace.v1"
)

// canonicalBuf accumulates the canonical byte stream.
type canonicalBuf struct {
	buf []byte
}

// str writes len(s) as a uvarint followed by the raw bytes of s.
func (c *canonicalBuf) str(s string) {
	c.buf = binary.AppendUvarint(c.buf, uint64(len(s)))
	c.buf = append(c.buf, s...)
}

// i64 writes n as 8 big-endian bytes — fixed width, so no value of one field
// can be mistaken for the start of the next.
func (c *canonicalBuf) i64(n int64) {
	c.buf = binary.BigEndian.AppendUint64(c.buf, uint64(n))
}

// count writes the length of a list as a uvarint.
func (c *canonicalBuf) count(n int) {
	c.buf = binary.AppendUvarint(c.buf, uint64(n))
}

// blob writes an already-canonical sub-encoding, length-prefixed.
func (c *canonicalBuf) blob(b []byte) {
	c.buf = binary.AppendUvarint(c.buf, uint64(len(b)))
	c.buf = append(c.buf, b...)
}

// canonical renders the repo view as canonical bytes.
func (v RepoViewID) canonical() []byte {
	var c canonicalBuf
	c.str(repoViewTag)
	c.str(v.RepoPrefix)
	c.str(v.BaseGraphID)
	c.i64(v.BaseGeneration)
	c.count(len(v.Layers))
	for _, l := range v.Layers {
		c.str(string(l.Kind))
		c.str(l.LayerID)
		c.i64(l.Generation)
		c.str(l.BufferFingerprint)
	}
	return c.buf
}

// canonical renders the workspace view as canonical bytes, sorting a copy of
// the repos so storage order cannot change the result.
func (v WorkspaceViewID) canonical() []byte {
	sorted := slices.Clone(v.Repos)
	slices.SortStableFunc(sorted, compareRepoViews)
	var c canonicalBuf
	c.str(workspaceViewTag)
	c.count(len(sorted))
	for _, r := range sorted {
		c.blob(r.canonical())
	}
	return c.buf
}

// hashCanonical reduces a canonical encoding to a lowercase hex sha256.
func hashCanonical(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ViewFileScheme is the URI scheme a file location under a pinned view is
// reported with.
const ViewFileScheme = "gortex-view"

// ViewFileURI names one file inside one view:
// gortex-view://<view-fingerprint>/<repo-prefix>/<percent-encoded-path>.
//
// A view built from a committed tree has no filesystem root, so there is no
// absolute path to report and reporting one from the canonical checkout would
// name bytes the view never read. The fingerprint is what makes the location
// resolvable: it identifies the exact content, and the path is relative to the
// repository inside it. Every path segment is percent-encoded, so a name
// carrying a slash-adjacent character survives the round trip.
func ViewFileURI(fingerprint, repoPrefix, relPath string) string {
	var b strings.Builder
	b.WriteString(ViewFileScheme)
	b.WriteString("://")
	b.WriteString(url.PathEscape(fingerprint))
	for _, segment := range strings.Split(path.Join(repoPrefix, relPath), "/") {
		if segment == "" {
			continue
		}
		b.WriteByte('/')
		b.WriteString(url.PathEscape(segment))
	}
	return b.String()
}
