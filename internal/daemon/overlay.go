package daemon

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"
)

// DefaultOverlayIdleTTL is the default per-session idle expiry for
// editor-buffer overlays. Raised from the original 5-minute draft to
// 30 minutes after field feedback: realistic coding sessions
// (deliberation, debugger pauses, IDE refactor wizards) commonly
// exceed five minutes without producing a Push, and the read-time
// LastUsed bump in SnapshotFor already keeps the session alive for
// any tool call. Thirty minutes is comfortably above typical
// inactivity gaps and well below the point where a forgotten
// disconnected client would meaningfully pin memory.
const DefaultOverlayIdleTTL = 30 * time.Minute

// MainBranchName is the implicit base branch every session starts on.
// All existing overlay tools (push / list / delete / drop /
// compare_with_overlay / preview_edit / simulate_chain) operate
// against the active branch — and unless the caller explicitly
// switches, that branch is `main`. Existing callers that never touch
// the branch tools see one branch with this name and never need to
// know about branches at all.
const MainBranchName = "main"

// branchNamePattern bounds branch names to a small, predictable
// alphabet. Stays well clear of shell-meta and path-separator
// characters so a branch name can safely embed in tool responses,
// log lines, or future on-disk representations.
var branchNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]{0,63}$`)

// OverlayIdleTTLFromEnv resolves the idle-TTL setting that
// `gortex server` / `gortex daemon` should construct their
// OverlayManager with. Precedence:
//
//  1. Caller-supplied override (non-zero).
//  2. `GORTEX_OVERLAY_IDLE_TTL` env var, parsed by time.ParseDuration
//     ("30m", "1h", "45s", etc.).
//  3. DefaultOverlayIdleTTL.
//
// A negative parse result clamps to 0 (which means "no expiry" —
// useful only for tests). Garbage env values fall back to the
// default; we deliberately don't fail startup over a typo'd duration.
func OverlayIdleTTLFromEnv(override time.Duration) time.Duration {
	if override > 0 {
		return override
	}
	if raw := os.Getenv("GORTEX_OVERLAY_IDLE_TTL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			if d < 0 {
				d = 0
			}
			return d
		}
	}
	return DefaultOverlayIdleTTL
}

// OverlayFile is one editor-buffer override pushed by an MCP client.
// The daemon (or a remote graph service over the gateway) merges
// these on top of the base graph view for the duration of a session.
// Iteration 1 only models text files; binary overlays would need a
// different shape and are not in scope.
type OverlayFile struct {
	// Path is repo-relative when WorkspaceID is set, absolute
	// otherwise. The graph service maps it onto the repo root via
	// its ConfigManager.
	Path string `json:"path"`
	// Content is the in-editor text. Empty means "deletion overlay"
	// — the client wants the daemon to act as if the file isn't on
	// disk, even if it actually is. The daemon distinguishes this
	// from a real empty file by only honouring deletions when the
	// session declares them via OverlayPush(..., Deleted: true).
	Content string `json:"content"`
	// BaseSHA is the file's git blob SHA at the time the editor
	// opened it. Used by the daemon's drift-detection: if the file
	// on disk now hashes to a different SHA, the overlay is stale
	// and the daemon refuses to merge it (returns ErrOverlayDrift).
	// Empty disables drift-detection — useful for editor-buffer
	// states that exist before any save.
	BaseSHA string `json:"base_sha,omitempty"`
	// Deleted, when true, marks the overlay as a tombstone (see
	// Content above). Mutually exclusive with non-empty Content.
	Deleted bool `json:"deleted,omitempty"`
}

// overlayBranch is one named speculative state owned by an
// OverlaySession. A session always carries the implicit `main`
// branch; `overlay_fork` materialises additional branches by deep-
// copying the source branch's file map. Branches share nothing
// mutable after the fork instant — every Push / Delete / Drop
// targets exactly one branch and leaves the rest untouched.
type overlayBranch struct {
	Name    string
	Parent  string // empty for the implicit `main` branch
	Created time.Time
	files   map[string]OverlayFile // path → overlay
}

// OverlaySession holds one client's pushed overlays for the duration
// of an MCP session. Sessions auto-expire after IdleTTL of inactivity
// so a crashed client doesn't leak memory in the daemon. Each session
// carries a set of named branches (always at least the implicit
// `main` branch) and exactly one is active at any time; all existing
// overlay queries route to that active branch.
type OverlaySession struct {
	ID          string
	WorkspaceID string
	Created     time.Time
	LastUsed    time.Time
	branches    map[string]*overlayBranch
	active      string
}

// activeBranch returns the active branch struct. Callers always hold
// the manager's mutex when invoking this — branches mutate exclusively
// under it.
func (s *OverlaySession) activeBranch() *overlayBranch {
	if s == nil {
		return nil
	}
	if br, ok := s.branches[s.active]; ok {
		return br
	}
	// Defensive fallback: surfaces an empty `main` branch if the
	// active pointer somehow drifted. RegisterWithID / SwitchBranch
	// keep `active` in sync with branches; this guard is belt-and-
	// braces for future refactors.
	if br, ok := s.branches[MainBranchName]; ok {
		s.active = MainBranchName
		return br
	}
	return nil
}

// OverlayManager manages the per-session overlay map for the daemon.
// Goroutine-safe; callers can register, push, and delete from any
// goroutine. A single janitor goroutine sweeps idle sessions.
type OverlayManager struct {
	mu       sync.RWMutex
	sessions map[string]*OverlaySession
	idleTTL  time.Duration
}

// ErrSessionNotFound is returned by OverlayManager methods that
// reference an unknown session ID. The daemon translates this to
// HTTP 404 on `/v1/overlay/<id>/...` endpoints.
var ErrSessionNotFound = errors.New("overlay session not found")

// ErrOverlayDrift is returned by OverlayPush when the supplied
// BaseSHA disagrees with the file's current on-disk SHA. The client
// is expected to re-read the file and resubmit a fresh overlay; the
// daemon refuses to fold a known-stale overlay into queries because
// merge artefacts (lines moved by a sibling tool's edit) would
// surface as wrong-line errors that look like graph bugs.
var ErrOverlayDrift = errors.New("overlay base SHA mismatch — re-read and resubmit")

// ErrOverlaySnapshotTooLarge reports that a bounded overlay snapshot could not
// be materialized within its caller-supplied file or byte envelope. Resource is
// either "files" or "bytes" and Limit is the corresponding hard cap. Bounded
// snapshot APIs return no workspace or file slice with this error.
type ErrOverlaySnapshotTooLarge struct {
	Resource string
	Limit    int
}

func (e *ErrOverlaySnapshotTooLarge) Error() string {
	return fmt.Sprintf("overlay snapshot too large: %s exceed limit %d", e.Resource, e.Limit)
}

// ErrBranchNotFound is returned by branch tools when the named
// branch does not exist on the session. The MCP surface translates
// this into a structured tool error with the branch name embedded.
var ErrBranchNotFound = errors.New("overlay branch not found")

// ErrBranchExists is returned by Fork when the destination name is
// already taken. Branch names are unique per session.
var ErrBranchExists = errors.New("overlay branch already exists")

// ErrInvalidBranchName is returned when the caller passes a name
// that doesn't match branchNamePattern. The MCP surface echoes the
// pattern back so callers can correct the request locally.
var ErrInvalidBranchName = errors.New("invalid overlay branch name (alphanumeric + dash/underscore, max 64 chars)")

// ErrCannotDropActiveBranch is returned by DropBranch when the
// caller targets the currently active branch. The caller must
// SwitchBranch off it first.
var ErrCannotDropActiveBranch = errors.New("cannot drop the active overlay branch — switch first")

// ErrCannotDropMainBranch refuses to delete the implicit `main`
// branch. The session's base must always exist; dropping the whole
// session is the right move when the caller wants to walk away.
var ErrCannotDropMainBranch = errors.New("cannot drop the implicit main overlay branch — drop the session instead")

// ErrMergeConflict signals that a non-forced merge tried to fold
// branches that disagree on at least one file's content. The MCP
// surface returns the conflicting paths to the caller so they can
// resolve and retry with force:true.
var ErrMergeConflict = errors.New("overlay merge conflict — pass force:true to override (last-writer-wins)")

// NewOverlayManager creates a manager with the given idle TTL. ttl
// <= 0 disables expiry (useful for tests that want deterministic
// session behaviour).
func NewOverlayManager(idleTTL time.Duration) *OverlayManager {
	return &OverlayManager{
		sessions: make(map[string]*OverlaySession),
		idleTTL:  idleTTL,
	}
}

// Register starts a new session and returns its ID. The workspace
// slug is captured at register time; later pushes that target a
// different workspace are rejected (one session = one workspace,
// per the overlay model).
func (m *OverlayManager) Register(workspaceID string) string {
	id := newSessionID()
	_ = m.RegisterWithID(id, workspaceID)
	return id
}

// ErrSessionExists is returned by RegisterWithID when the caller-supplied
// session ID is already known. The MCP-side overlay tools rely on this
// error to detect "register-called-twice" races; HTTP callers don't see
// it because Register generates fresh IDs.
var ErrSessionExists = errors.New("overlay session already exists")

// RegisterWithID registers a session under a caller-chosen ID. This is
// the path the MCP `overlay_register` tool takes — it binds the overlay
// session to the MCP session ID so the query path can find the overlay
// snapshot from the request context without an extra lookup. The HTTP
// register handler also routes through here when its body includes an
// explicit `session_id`.
//
// Returns ErrSessionExists when the ID is already in use. Idempotent
// re-registration (same workspaceID) is treated as a no-op: the client
// may safely retry register without first checking.
func (m *OverlayManager) RegisterWithID(sessionID, workspaceID string) error {
	if sessionID == "" {
		return errors.New("overlay session id is required")
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.sessions[sessionID]; ok {
		if existing.WorkspaceID == workspaceID {
			existing.LastUsed = now
			return nil
		}
		return ErrSessionExists
	}
	m.sessions[sessionID] = &OverlaySession{
		ID:          sessionID,
		WorkspaceID: workspaceID,
		Created:     now,
		LastUsed:    now,
		branches: map[string]*overlayBranch{
			MainBranchName: {
				Name:    MainBranchName,
				Created: now,
				files:   make(map[string]OverlayFile),
			},
		},
		active: MainBranchName,
	}
	return nil
}

// Has reports whether a session is currently registered. Used by the
// MCP tool dispatcher to decide whether to skip the per-request apply
// pass (no session → no work). Cheap O(1) read under the read lock.
func (m *OverlayManager) Has(sessionID string) bool {
	if m == nil || sessionID == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.sessions[sessionID]
	return ok
}

// FileCount returns the number of overlay files attached to a session's
// active branch, 0 if the session is unknown. Cheap fast-path: lets the
// dispatcher skip the apply pass when a session is registered but its
// active branch is empty.
func (m *OverlayManager) FileCount(sessionID string) int {
	if m == nil || sessionID == "" {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return 0
	}
	br := sess.activeBranch()
	if br == nil {
		return 0
	}
	return len(br.files)
}

// Touch refreshes a session's idle timer without altering its overlay
// files. Called by the MCP `overlay_keepalive` tool so an editor can
// explicitly extend the lease without re-pushing buffer content.
// Returns ErrSessionNotFound when the session doesn't exist so the
// keepalive tool can surface "session lost; please re-register".
func (m *OverlayManager) Touch(sessionID string) error {
	if m == nil {
		return ErrSessionNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	sess.LastUsed = time.Now()
	return nil
}

// SessionStatus is the per-session liveness snapshot reported through
// overlay_list. Callers compare `IdleSeconds` to `IdleTTLSeconds` to
// decide when to push a keepalive; `ExpiresAt` (RFC3339) is the
// concrete wall-clock instant the janitor would reap the session if
// no further activity arrived. ActiveBranch carries the session's
// currently-active branch name (always non-empty for a live session).
type SessionStatus struct {
	WorkspaceID    string
	Created        time.Time
	LastUsed       time.Time
	IdleSeconds    float64
	IdleTTLSeconds float64
	ExpiresAt      time.Time // zero when idleTTL <= 0
	ActiveBranch   string
}

// StatusFor returns liveness metadata for a session without touching
// LastUsed (unlike SnapshotFor, which is a read-with-bump). Used by
// overlay_list to render expiry hints without resetting the timer
// every time the editor polls.
func (m *OverlayManager) StatusFor(sessionID string) (SessionStatus, error) {
	if m == nil {
		return SessionStatus{}, ErrSessionNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return SessionStatus{}, ErrSessionNotFound
	}
	st := SessionStatus{
		WorkspaceID:    sess.WorkspaceID,
		Created:        sess.Created,
		LastUsed:       sess.LastUsed,
		IdleSeconds:    time.Since(sess.LastUsed).Seconds(),
		IdleTTLSeconds: m.idleTTL.Seconds(),
		ActiveBranch:   sess.active,
	}
	if m.idleTTL > 0 {
		st.ExpiresAt = sess.LastUsed.Add(m.idleTTL)
	}
	return st, nil
}

// SnapshotFor returns the overlay files for a session's active branch
// in a stable, path-sorted order along with the workspace slug
// captured at register time. Returns ErrSessionNotFound when the
// session doesn't exist. The returned slice never aliases the
// manager's internal map; callers can mutate it freely.
//
// This is the preferred read API for the query path: the deterministic
// ordering means two overlay-active requests with the same overlay set
// touch the same paths in the same order, which simplifies test
// assertions and makes drift errors point at the same path on retry.
func (m *OverlayManager) SnapshotFor(sessionID string) (workspace string, files []OverlayFile, err error) {
	if m == nil {
		return "", nil, ErrSessionNotFound
	}
	// Ordinary MCP sessions are not overlay sessions. Prove absence under a
	// shared lock so their request preparation never queues for the manager-wide
	// writer. A Register racing after this check belongs to the next request;
	// this read-lock instant is the current request's empty snapshot boundary.
	m.mu.RLock()
	_, exists := m.sessions[sessionID]
	m.mu.RUnlock()
	if !exists {
		return "", nil, ErrSessionNotFound
	}

	// Registered sessions still count reads as activity. Re-check after lock
	// promotion because Drop may win the gap, then copy the active branch while
	// protected. Sorting happens after unlock: the returned slice owns its
	// headers and no longer needs to serialize unrelated sessions.
	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return "", nil, ErrSessionNotFound
	}
	sess.LastUsed = time.Now()
	workspace = sess.WorkspaceID
	br := sess.activeBranch()
	if br == nil {
		m.mu.Unlock()
		return workspace, nil, nil
	}
	out := make([]OverlayFile, 0, len(br.files))
	for _, f := range br.files {
		out = append(out, f)
	}
	m.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return workspace, out, nil
}

// SnapshotForBounded is SnapshotFor with a hard pre-materialization envelope.
// Both limits must be positive. File count and Path+Content+BaseSHA bytes are
// checked while holding the manager lock and before allocating the result
// slice. LastUsed is bumped even when the envelope rejects the snapshot.
func (m *OverlayManager) SnapshotForBounded(
	sessionID string,
	maxFiles, maxBytes int,
) (workspace string, files []OverlayFile, err error) {
	if maxFiles <= 0 || maxBytes <= 0 {
		return "", nil, fmt.Errorf("overlay snapshot limits must be positive: files=%d bytes=%d", maxFiles, maxBytes)
	}
	if m == nil {
		return "", nil, ErrSessionNotFound
	}

	m.mu.RLock()
	_, exists := m.sessions[sessionID]
	m.mu.RUnlock()
	if !exists {
		return "", nil, ErrSessionNotFound
	}

	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return "", nil, ErrSessionNotFound
	}
	sess.LastUsed = time.Now()
	workspace = sess.WorkspaceID
	br := sess.activeBranch()
	if br == nil {
		m.mu.Unlock()
		return workspace, nil, nil
	}
	out, snapshotErr := copyOverlayFilesBounded(br.files, maxFiles, maxBytes)
	m.mu.Unlock()
	if snapshotErr != nil {
		return "", nil, snapshotErr
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return workspace, out, nil
}

// copyOverlayFilesBounded validates a locked branch map before allocating or
// copying its snapshot. Callers must hold the manager lock for the full call.
func copyOverlayFilesBounded(
	files map[string]OverlayFile,
	maxFiles, maxBytes int,
) ([]OverlayFile, error) {
	if len(files) > maxFiles {
		return nil, &ErrOverlaySnapshotTooLarge{Resource: "files", Limit: maxFiles}
	}
	totalBytes := 0
	for _, file := range files {
		for _, size := range [...]int{len(file.Path), len(file.Content), len(file.BaseSHA)} {
			if size > maxBytes-totalBytes {
				return nil, &ErrOverlaySnapshotTooLarge{Resource: "bytes", Limit: maxBytes}
			}
			totalBytes += size
		}
	}
	out := make([]OverlayFile, 0, len(files))
	for _, file := range files {
		out = append(out, file)
	}
	return out, nil
}

// Push attaches one overlay file to a session's active branch.
// Workspace mismatch (the session was registered for workspace X but
// the push targets Y) returns an error: a session is supposed to be
// a coherent view over one workspace's repos.
//
// driftCheck is a callback the manager invokes to verify BaseSHA
// against the on-disk file. The daemon supplies it; tests can pass
// nil to skip the check. If driftCheck returns false and overlay
// has a non-empty BaseSHA, Push fails with ErrOverlayDrift.
func (m *OverlayManager) Push(sessionID string, overlay OverlayFile, driftCheck func(path, sha string) bool) error {
	if overlay.Path == "" {
		return errors.New("overlay path is required")
	}
	if overlay.Deleted && overlay.Content != "" {
		return errors.New("overlay cannot be both deleted and have content")
	}
	if overlay.BaseSHA != "" && driftCheck != nil {
		if !driftCheck(overlay.Path, overlay.BaseSHA) {
			return ErrOverlayDrift
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	br := sess.activeBranch()
	if br == nil {
		return ErrSessionNotFound
	}
	br.files[overlay.Path] = overlay
	sess.LastUsed = time.Now()
	return nil
}

// Delete removes one overlay file from a session's active branch by
// path. Returns ErrSessionNotFound when the session doesn't exist.
func (m *OverlayManager) Delete(sessionID, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	br := sess.activeBranch()
	if br == nil {
		return ErrSessionNotFound
	}
	delete(br.files, path)
	sess.LastUsed = time.Now()
	return nil
}

// Drop terminates the session and discards every branch and overlay
// it held. Idempotent — dropping an unknown session is a no-op.
func (m *OverlayManager) Drop(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}

// Files returns a snapshot of every overlay attached to a session's
// active branch (no live aliasing — the returned map can be mutated
// freely). ErrSessionNotFound when the session doesn't exist.
func (m *OverlayManager) Files(sessionID string) (map[string]OverlayFile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	br := sess.activeBranch()
	if br == nil {
		return map[string]OverlayFile{}, nil
	}
	out := make(map[string]OverlayFile, len(br.files))
	for k, v := range br.files {
		out[k] = v
	}
	return out, nil
}

// SweepIdle drops sessions whose LastUsed is older than IdleTTL.
// Returns the count of dropped sessions for telemetry. Safe to call
// from a single janitor goroutine on a ticker.
func (m *OverlayManager) SweepIdle() int {
	if m.idleTTL <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-m.idleTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	dropped := 0
	for id, sess := range m.sessions {
		if sess.LastUsed.Before(cutoff) {
			delete(m.sessions, id)
			dropped++
		}
	}
	return dropped
}

// StartJanitor launches the background sweeper that enforces IdleTTL.
// Without it SweepIdle never runs and a session whose proxy vanished
// without an observed disconnect pins its pushed buffers for the
// daemon's lifetime. interval <= 0 derives a default from the TTL
// (TTL/4, floored at one minute). onSweep, when non-nil, is invoked
// with the dropped count after any sweep that removed sessions —
// telemetry stays the caller's concern. The returned stop func
// terminates the goroutine and is safe to call more than once. A
// manager with expiry disabled (idleTTL <= 0) starts no goroutine
// and returns a no-op stop.
func (m *OverlayManager) StartJanitor(interval time.Duration, onSweep func(dropped int)) (stop func()) {
	if m.idleTTL <= 0 {
		return func() {}
	}
	if interval <= 0 {
		interval = m.idleTTL / 4
		if interval < time.Minute {
			interval = time.Minute
		}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if dropped := m.SweepIdle(); dropped > 0 && onSweep != nil {
					onSweep(dropped)
				}
			case <-done:
				return
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// ---------------------------------------------------------------------------
// Branching surface: fork, list, switch, merge, drop_branch.
// ---------------------------------------------------------------------------

// ValidateBranchName returns ErrInvalidBranchName when the supplied
// name does not satisfy the [A-Za-z0-9][A-Za-z0-9_-]{0,63} pattern.
// Exposed so the MCP layer can fail fast with a precise message
// before locking the manager.
func ValidateBranchName(name string) error {
	if !branchNamePattern.MatchString(name) {
		return ErrInvalidBranchName
	}
	return nil
}

// BranchInfo is the per-branch row returned by Branches.
type BranchInfo struct {
	Name         string
	Active       bool
	FileCount    int
	BaseSHACount int
	Parent       string
	Created      time.Time
}

// Branches lists every branch a session holds, including the implicit
// `main` branch. The returned slice is sorted by name (with the
// active branch always first in the response when the caller wants
// to surface it). Returns ErrSessionNotFound when the session doesn't
// exist.
func (m *OverlayManager) Branches(sessionID string) ([]BranchInfo, error) {
	if m == nil {
		return nil, ErrSessionNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	out := make([]BranchInfo, 0, len(sess.branches))
	for name, br := range sess.branches {
		baseShas := 0
		for _, f := range br.files {
			if f.BaseSHA != "" {
				baseShas++
			}
		}
		out = append(out, BranchInfo{
			Name:         name,
			Active:       name == sess.active,
			FileCount:    len(br.files),
			BaseSHACount: baseShas,
			Parent:       br.Parent,
			Created:      br.Created,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		// Active branch first, then alphabetical — keeps the JSON
		// payload deterministic AND foregrounds the most relevant
		// entry for diagnostic dumps.
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ActiveBranch returns the name of the session's active branch.
// Returns ErrSessionNotFound when the session doesn't exist.
func (m *OverlayManager) ActiveBranch(sessionID string) (string, error) {
	if m == nil {
		return "", ErrSessionNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return "", ErrSessionNotFound
	}
	return sess.active, nil
}

// ForkOptions controls the Fork call. Name is required; From defaults
// to the session's active branch; Activate makes the new branch the
// session's active branch (otherwise the caller must SwitchBranch
// explicitly to use it).
type ForkOptions struct {
	Name     string
	From     string // empty → session's active branch
	Activate bool
}

// ForkResult carries the post-fork state so the MCP surface can echo
// it back to the caller without a separate Branches call.
type ForkResult struct {
	Branch    string
	Parent    string
	FileCount int
	Active    bool
}

// Fork clones the source branch into a new named branch on the same
// session. The new branch starts with a deep copy of the source's
// file map; no shared mutable state remains after the fork instant.
// Returns ErrBranchExists when the destination name is already taken.
func (m *OverlayManager) Fork(sessionID string, opts ForkOptions) (ForkResult, error) {
	if m == nil {
		return ForkResult{}, ErrSessionNotFound
	}
	if err := ValidateBranchName(opts.Name); err != nil {
		return ForkResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ForkResult{}, ErrSessionNotFound
	}
	if _, ok := sess.branches[opts.Name]; ok {
		return ForkResult{}, ErrBranchExists
	}
	source := opts.From
	if source == "" {
		source = sess.active
	}
	src, ok := sess.branches[source]
	if !ok {
		return ForkResult{}, ErrBranchNotFound
	}
	cloneFiles := make(map[string]OverlayFile, len(src.files))
	for k, v := range src.files {
		cloneFiles[k] = v
	}
	now := time.Now()
	sess.branches[opts.Name] = &overlayBranch{
		Name:    opts.Name,
		Parent:  source,
		Created: now,
		files:   cloneFiles,
	}
	if opts.Activate {
		sess.active = opts.Name
	}
	sess.LastUsed = now
	return ForkResult{
		Branch:    opts.Name,
		Parent:    source,
		FileCount: len(cloneFiles),
		Active:    sess.active == opts.Name,
	}, nil
}

// SwitchBranch flips the session's active branch. Returns
// ErrBranchNotFound when the name is unknown.
func (m *OverlayManager) SwitchBranch(sessionID, name string) error {
	if m == nil {
		return ErrSessionNotFound
	}
	if err := ValidateBranchName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	if _, ok := sess.branches[name]; !ok {
		return ErrBranchNotFound
	}
	sess.active = name
	sess.LastUsed = time.Now()
	return nil
}

// DropBranch deletes a named branch. Refuses to drop the active
// branch (caller must SwitchBranch first) and refuses to drop the
// implicit `main` branch (which would leave the session in an
// inconsistent state — dropping the whole session is the right move
// when the caller wants to walk away).
func (m *OverlayManager) DropBranch(sessionID, name string) error {
	if m == nil {
		return ErrSessionNotFound
	}
	if name == MainBranchName {
		return ErrCannotDropMainBranch
	}
	if err := ValidateBranchName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	if name == sess.active {
		return ErrCannotDropActiveBranch
	}
	if _, ok := sess.branches[name]; !ok {
		return ErrBranchNotFound
	}
	delete(sess.branches, name)
	sess.LastUsed = time.Now()
	return nil
}

// MergeOptions controls MergeBranches. From and Into name the source
// and destination branches; ToDisk redirects the merge target from
// `Into` to the filesystem (writing each From file with the supplied
// writer callback). Force flips conflict policy from "refuse" to
// "last-writer-wins" — without it, any path that exists in both
// branches with different content blocks the merge.
type MergeOptions struct {
	From   string
	Into   string
	ToDisk bool
	Force  bool
	// ExpectedFiles is a bounded source snapshot checked under the branch
	// mutex before the first disk callback. Nil retains legacy callers;
	// a non-nil empty slice requires an empty source branch.
	ExpectedFiles []OverlayFile
}

// DiskWriteFn is the callback MergeBranches invokes for each From
// file when ToDisk is true. The implementer is responsible for the
// drift check (re-hash the on-disk content, compare to BaseSHA,
// return ErrOverlayDrift on mismatch) and the atomic write. Keeping
// the daemon free of filesystem and SHA-hashing concerns avoids a
// circular dependency on `internal/mcp` helpers; the MCP layer
// injects its existing `gitBlobSHA` + `agents.AtomicWriteFile`
// helpers as the writer.
type DiskWriteFn func(file OverlayFile) error

// MergeResult is the structured outcome of MergeBranches. The MCP
// surface echoes it back to the caller; the `Conflicts` slice
// carries the paths that disagreed when Force was false (the merge
// aborted) or that were last-writer-wins-resolved when Force was true.
type MergeResult struct {
	Merged          int
	Conflicts       []string
	DroppedBranch   bool
	ResolvedByForce bool
}

// MergeBranches folds opts.From into opts.Into (or onto disk when
// opts.ToDisk is true). Conflict policy: a "conflict" is a path that
// exists in both branches with different content/deleted/baseSha
// tuples. Without Force, conflicts abort the merge and the result
// reports the conflicting paths. With Force, conflicts are
// last-writer-wins: opts.From wins (consistent with the source-
// branch-driven model) and the response notes the resolution.
//
// When ToDisk is true, opts.From is written to the filesystem via the
// caller-supplied DiskWriteFn (which is expected to enforce the
// BaseSHA drift guard) and opts.From is dropped from the session on
// success. ToDisk + Force is meaningful: with drift-check enforcement
// inside the writer, force only affects branch-vs-branch conflict
// resolution; the writer itself is the disk-drift authority.
func (m *OverlayManager) MergeBranches(sessionID string, opts MergeOptions, write DiskWriteFn) (MergeResult, error) {
	if m == nil {
		return MergeResult{}, ErrSessionNotFound
	}
	if opts.From == "" {
		return MergeResult{}, errors.New("merge: from branch is required")
	}
	if err := ValidateBranchName(opts.From); err != nil {
		return MergeResult{}, err
	}
	if !opts.ToDisk {
		into := opts.Into
		if into == "" {
			into = MainBranchName
		}
		if err := ValidateBranchName(into); err != nil {
			return MergeResult{}, err
		}
		opts.Into = into
		if opts.From == opts.Into {
			return MergeResult{}, errors.New("merge: from and into are the same branch")
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return MergeResult{}, ErrSessionNotFound
	}
	src, ok := sess.branches[opts.From]
	if !ok {
		return MergeResult{}, ErrBranchNotFound
	}

	if opts.ToDisk {
		if opts.ExpectedFiles != nil {
			if len(opts.ExpectedFiles) != len(src.files) {
				return MergeResult{}, errors.New("merge: source branch changed while waiting for disk paths")
			}
			for _, expected := range opts.ExpectedFiles {
				actual, ok := src.files[expected.Path]
				if !ok || !overlayFilesEqual(actual, expected) {
					return MergeResult{}, errors.New("merge: source branch changed while waiting for disk paths")
				}
			}
		}
		if write == nil {
			return MergeResult{}, errors.New("merge: disk write callback is required for to_disk merges")
		}
		// Iterate paths in deterministic order so a drift error
		// always points at the same path on retry.
		paths := make([]string, 0, len(src.files))
		for p := range src.files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		merged := 0
		for _, p := range paths {
			f := src.files[p]
			if err := write(f); err != nil {
				// Surface drift unchanged so callers can pattern-
				// match the existing ErrOverlayDrift string.
				return MergeResult{Merged: merged}, err
			}
			merged++
		}
		delete(sess.branches, opts.From)
		if sess.active == opts.From {
			sess.active = MainBranchName
		}
		sess.LastUsed = time.Now()
		return MergeResult{Merged: merged, DroppedBranch: true}, nil
	}

	dst, ok := sess.branches[opts.Into]
	if !ok {
		return MergeResult{}, ErrBranchNotFound
	}

	// Walk From in deterministic order so the conflict / merge
	// list and the response are stable across runs.
	paths := make([]string, 0, len(src.files))
	for p := range src.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var conflicts []string
	for _, p := range paths {
		fromFile := src.files[p]
		if existing, ok := dst.files[p]; ok && !overlayFilesEqual(existing, fromFile) {
			conflicts = append(conflicts, p)
		}
	}
	if len(conflicts) > 0 && !opts.Force {
		return MergeResult{Conflicts: conflicts}, ErrMergeConflict
	}

	merged := 0
	for _, p := range paths {
		fromFile := src.files[p]
		dst.files[p] = fromFile
		merged++
	}
	sess.LastUsed = time.Now()
	return MergeResult{
		Merged:          merged,
		Conflicts:       conflicts,
		ResolvedByForce: len(conflicts) > 0,
	}, nil
}

// overlayFilesEqual is the conflict-policy primitive: two files
// disagree when any user-visible field differs. BaseSHA is included
// because two branches that opened the same disk file at different
// times may have stale SHAs that, if silently merged, would carry
// a known-stale baseline into the destination.
func overlayFilesEqual(a, b OverlayFile) bool {
	return a.Path == b.Path &&
		a.Content == b.Content &&
		a.BaseSHA == b.BaseSHA &&
		a.Deleted == b.Deleted
}

// PushToBranch attaches one overlay file to a *specific* branch
// without changing the session's active branch. The single-branch
// Push API operates against the active branch (backward compat);
// this variant is for callers — primarily test harnesses and
// future concurrent agents — that need to mutate sibling branches
// without disturbing the active pointer. Conflict semantics with
// the active-branch view are identical: a push merely overwrites
// the per-branch file map under the manager's mutex.
func (m *OverlayManager) PushToBranch(sessionID, branchName string, overlay OverlayFile, driftCheck func(path, sha string) bool) error {
	if overlay.Path == "" {
		return errors.New("overlay path is required")
	}
	if overlay.Deleted && overlay.Content != "" {
		return errors.New("overlay cannot be both deleted and have content")
	}
	if overlay.BaseSHA != "" && driftCheck != nil {
		if !driftCheck(overlay.Path, overlay.BaseSHA) {
			return ErrOverlayDrift
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	br, ok := sess.branches[branchName]
	if !ok {
		return ErrBranchNotFound
	}
	br.files[overlay.Path] = overlay
	sess.LastUsed = time.Now()
	return nil
}

// FilesForBranch returns the file map for a specific branch. Used by
// compare_branches (which has to read both branches in one shot to
// compute the delta). The returned slice never aliases the manager's
// internal state. Returns ErrBranchNotFound when the branch is
// unknown.
func (m *OverlayManager) FilesForBranch(sessionID, branchName string) ([]OverlayFile, error) {
	if m == nil {
		return nil, ErrSessionNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	br, ok := sess.branches[branchName]
	if !ok {
		return nil, ErrBranchNotFound
	}
	sess.LastUsed = time.Now()
	out := make([]OverlayFile, 0, len(br.files))
	for _, f := range br.files {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// FilesForBranchBounded snapshots a named branch under the same bounded
// materialization contract as SnapshotForBounded.
func (m *OverlayManager) FilesForBranchBounded(
	sessionID, branchName string,
	maxFiles, maxBytes int,
) ([]OverlayFile, error) {
	if maxFiles <= 0 || maxBytes <= 0 {
		return nil, fmt.Errorf("overlay snapshot limits must be positive: files=%d bytes=%d", maxFiles, maxBytes)
	}
	if m == nil {
		return nil, ErrSessionNotFound
	}

	m.mu.RLock()
	_, exists := m.sessions[sessionID]
	m.mu.RUnlock()
	if !exists {
		return nil, ErrSessionNotFound
	}

	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return nil, ErrSessionNotFound
	}
	br, ok := sess.branches[branchName]
	if !ok {
		m.mu.Unlock()
		return nil, ErrBranchNotFound
	}
	sess.LastUsed = time.Now()
	out, snapshotErr := copyOverlayFilesBounded(br.files, maxFiles, maxBytes)
	m.mu.Unlock()
	if snapshotErr != nil {
		return nil, snapshotErr
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
