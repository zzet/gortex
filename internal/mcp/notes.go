package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

// maxNotesCap is the soft ceiling on stored notes per repo scope.
// Trimming honours pinned notes: the oldest non-pinned notes are shed
// first. Matches the prior gob.gz cap.
const maxNotesCap = 5000

// notesManager owns the session-memory side-store: thread-safe note
// CRUD backed by the SQLite sidecar DB. Mirrors the lifecycle of
// feedbackManager — one per server, init-once, sidecar-or-noop.
//
// The in-memory slice + scorers are unchanged from the gob.gz era;
// only the persistence layer changed: rows load into the slice on
// construction and each mutation writes its row(s) to the sidecar.
// When sidecar is nil the manager operates in-memory only (test
// fixtures, single-shot CLI calls with no cache dir).
type notesManager struct {
	mu      sync.Mutex
	store   persistence.NoteStore
	sidecar *persistence.SidecarStore
	repoKey string
}

// newNotesManager constructs a manager, lazily loading any existing
// notes from the sidecar. Empty cacheDir/repoPath yields a no-disk
// manager. The sidecar lives at <cacheDir>/sidecar.sqlite; any legacy
// notes.gob.gz under the per-repo cache subdir is imported once, then
// renamed to *.bak.
func newNotesManager(cacheDir, repoPath string) *notesManager {
	if cacheDir == "" || repoPath == "" {
		return &notesManager{}
	}
	sidecar, err := persistence.OpenSidecar(persistence.DefaultSidecarPath(cacheDir))
	if err != nil || sidecar == nil {
		return &notesManager{}
	}
	return newNotesManagerFromSidecar(sidecar, persistence.RepoCacheKey(repoPath), persistence.NotesDir(cacheDir, repoPath))
}

// newNotesManagerFromSidecar builds a notes manager bound to an
// already-open sidecar + repo key, importing legacyDir/notes.gob.gz
// once. Used by the daemon path where the sidecar is opened once and
// shared across managers.
func newNotesManagerFromSidecar(sidecar *persistence.SidecarStore, repoKey, legacyDir string) *notesManager {
	nm := &notesManager{sidecar: sidecar, repoKey: repoKey}
	if sidecar != nil {
		_ = sidecar.MigrateLegacyNotes(repoKey, legacyDir)
		if rows, err := sidecar.LoadNotesRows(repoKey); err == nil {
			nm.store.Entries = rows
		}
	}
	return nm
}

// NoteQueryFilter constrains a Query call. Zero-value fields disable
// the corresponding filter; tag matching is exact (case-insensitive).
type NoteQueryFilter struct {
	SessionID   string
	SymbolID    string
	FilePath    string
	Tag         string
	TextSearch  string // case-insensitive substring against Body
	Since       time.Time
	WorkspaceID string
	// WorkspaceIn is the set-valued form of WorkspaceID, for a session
	// bound to the repos its cwd contains: that shape spans several
	// workspaces and has no single slug, and an empty WorkspaceID means
	// "no filter", so without this such a session would read every
	// workspace's notes.
	WorkspaceIn map[string]bool
	ProjectID   string
	Pinned      *bool // nil = either; true = pinned only; false = unpinned only
	Limit       int   // 0 = no limit
}

// Save persists a new entry, returning the generated ID. The entry's
// Timestamp / UpdatedAt / ID fields are populated here so callers
// don't have to.
func (nm *notesManager) Save(entry persistence.NoteEntry) (string, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if entry.ID == "" {
		entry.ID = newNoteID()
	}
	now := time.Now().UTC()
	if entry.Timestamp.IsZero() {
		entry.Timestamp = now
	}
	entry.UpdatedAt = now

	// Dedupe & normalise auto-links + tags so repeated saves stay clean.
	entry.AutoLinks = dedupeStrings(entry.AutoLinks)
	entry.Tags = dedupeStrings(normaliseTags(entry.Tags))

	nm.store.Entries = append(nm.store.Entries, entry)
	if err := nm.persistLocked(entry); err != nil {
		return entry.ID, err
	}
	nm.trimLocked()
	return entry.ID, nil
}

// Update mutates an existing note by ID. Pass nil for a field you
// don't want to change. Returns os.ErrNotExist-shaped error when
// the ID is unknown.
func (nm *notesManager) Update(id string, body *string, tags []string, pinned *bool, addLinks []string) (persistence.NoteEntry, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	idx := nm.findLocked(id)
	if idx < 0 {
		return persistence.NoteEntry{}, fmt.Errorf("note %q not found", id)
	}
	e := nm.store.Entries[idx]
	if body != nil {
		e.Body = *body
	}
	if tags != nil {
		e.Tags = dedupeStrings(normaliseTags(tags))
	}
	if pinned != nil {
		e.Pinned = *pinned
	}
	if len(addLinks) > 0 {
		e.AutoLinks = dedupeStrings(append(append([]string{}, e.AutoLinks...), addLinks...))
	}
	e.UpdatedAt = time.Now().UTC()
	nm.store.Entries[idx] = e
	if err := nm.persistLocked(e); err != nil {
		return e, err
	}
	return e, nil
}

// Delete removes a note by ID. Idempotent — deleting an unknown ID
// is not an error (the post-condition "the note is gone" already holds).
func (nm *notesManager) Delete(id string) error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	idx := nm.findLocked(id)
	if idx < 0 {
		return nil
	}
	nm.store.Entries = append(nm.store.Entries[:idx], nm.store.Entries[idx+1:]...)
	if nm.sidecar == nil {
		return nil
	}
	return nm.sidecar.DeleteNote(nm.repoKey, id)
}

// Get returns a single note by ID, or (zero, false) when not found.
func (nm *notesManager) Get(id string) (persistence.NoteEntry, bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	idx := nm.findLocked(id)
	if idx < 0 {
		return persistence.NoteEntry{}, false
	}
	return nm.store.Entries[idx], true
}

// Query returns notes matching every set filter. Results are sorted
// newest-first (by UpdatedAt). Limit caps the slice after filtering.
func (nm *notesManager) Query(f NoteQueryFilter) []persistence.NoteEntry {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	var out []persistence.NoteEntry
	textNeedle := strings.ToLower(f.TextSearch)
	tagNeedle := strings.ToLower(f.Tag)

	for _, e := range nm.store.Entries {
		if f.SessionID != "" && e.SessionID != f.SessionID {
			continue
		}
		if f.WorkspaceID != "" && e.WorkspaceID != f.WorkspaceID {
			continue
		}
		if len(f.WorkspaceIn) > 0 && !f.WorkspaceIn[e.WorkspaceID] {
			continue
		}
		if f.ProjectID != "" && e.ProjectID != f.ProjectID {
			continue
		}
		if f.SymbolID != "" {
			if !noteReferencesSymbol(e, f.SymbolID) {
				continue
			}
		}
		if f.FilePath != "" && e.FilePath != f.FilePath {
			continue
		}
		if tagNeedle != "" && !hasTag(e.Tags, tagNeedle) {
			continue
		}
		if textNeedle != "" && !strings.Contains(strings.ToLower(e.Body), textNeedle) {
			continue
		}
		if !f.Since.IsZero() && e.UpdatedAt.Before(f.Since) {
			continue
		}
		if f.Pinned != nil && e.Pinned != *f.Pinned {
			continue
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// HasData reports whether the store holds at least one note.
func (nm *notesManager) HasData() bool {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	return len(nm.store.Entries) > 0
}

// Count returns the total number of stored notes.
func (nm *notesManager) Count() int {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	return len(nm.store.Entries)
}

func (nm *notesManager) findLocked(id string) int {
	for i := range nm.store.Entries {
		if nm.store.Entries[i].ID == id {
			return i
		}
	}
	return -1
}

// persistLocked writes a single note row to the sidecar. No-op for an
// in-memory-only manager. Callers hold nm.mu.
func (nm *notesManager) persistLocked(e persistence.NoteEntry) error {
	if nm.sidecar == nil {
		return nil
	}
	return nm.sidecar.UpsertNote(nm.repoKey, e)
}

// trimLocked enforces the soft cap (maxNotesCap) via a bounded DELETE
// on the sidecar, then reconciles the in-memory slice so it stays in
// sync. No-op when under cap or in-memory-only. Callers hold nm.mu.
func (nm *notesManager) trimLocked() {
	if nm.sidecar == nil || len(nm.store.Entries) <= maxNotesCap {
		return
	}
	if err := nm.sidecar.TrimNotes(nm.repoKey, maxNotesCap); err != nil {
		return
	}
	if rows, err := nm.sidecar.LoadNotesRows(nm.repoKey); err == nil {
		nm.store.Entries = rows
	}
}

// distillResult is the structured digest returned by DistillSession.
// The shape is stable across both the JSON and gcx wire formats.
type distillResult struct {
	SessionID   string             `json:"session_id"`
	NoteCount   int                `json:"note_count"`
	Window      distillWindow      `json:"window"`
	TopSymbols  []distillSymbolHit `json:"top_symbols"`
	TopFiles    []distillCountHit  `json:"top_files,omitempty"`
	TopTags     []distillCountHit  `json:"top_tags,omitempty"`
	Decisions   []distillExcerpt   `json:"decisions,omitempty"`
	PinnedNotes []distillExcerpt   `json:"pinned_notes,omitempty"`
	Recent      []distillExcerpt   `json:"recent,omitempty"`
	Summary     string             `json:"summary,omitempty"`
	Truncated   bool               `json:"truncated,omitempty"`
}

type distillWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type distillSymbolHit struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Kind  string `json:"kind,omitempty"`
	Path  string `json:"path,omitempty"`
	Count int    `json:"count"`
}

type distillCountHit struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type distillExcerpt struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	Tags      []string  `json:"tags,omitempty"`
	Symbol    string    `json:"symbol,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// distillOptions tunes DistillSession.
type distillOptions struct {
	MaxSymbols int
	MaxFiles   int
	MaxTags    int
	MaxRecent  int
	ExcerptCap int // max bytes of body to retain per excerpt
}

func defaultDistillOptions() distillOptions {
	return distillOptions{
		MaxSymbols: 10,
		MaxFiles:   10,
		MaxTags:    10,
		MaxRecent:  8,
		ExcerptCap: 240,
	}
}

// DistillSession aggregates the notes for a session (or, when
// sessionID is empty, the whole store filtered by workspace) into a
// digest the agent can paste back into context after compaction.
//
// resolveNode is consulted to enrich top-symbol entries with name /
// kind / path. Pass nil to skip enrichment — the digest still has IDs.
func (nm *notesManager) DistillSession(sessionID, workspaceID, projectID string, opts distillOptions, resolveNode func(string) *graph.Node) distillResult {
	if opts.MaxSymbols <= 0 {
		opts.MaxSymbols = 10
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 10
	}
	if opts.MaxTags <= 0 {
		opts.MaxTags = 10
	}
	if opts.MaxRecent <= 0 {
		opts.MaxRecent = 8
	}
	if opts.ExcerptCap <= 0 {
		opts.ExcerptCap = 240
	}

	notes := nm.Query(NoteQueryFilter{
		SessionID:   sessionID,
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
	})

	res := distillResult{
		SessionID: sessionID,
		NoteCount: len(notes),
	}
	if len(notes) == 0 {
		return res
	}

	// Window: oldest-first → newest-first; we need both ends.
	minT := notes[0].UpdatedAt
	maxT := notes[0].UpdatedAt
	for _, n := range notes {
		if n.UpdatedAt.Before(minT) {
			minT = n.UpdatedAt
		}
		if n.UpdatedAt.After(maxT) {
			maxT = n.UpdatedAt
		}
	}
	res.Window = distillWindow{From: minT, To: maxT}

	// Tallies.
	symCount := make(map[string]int)
	fileCount := make(map[string]int)
	tagCount := make(map[string]int)
	for _, n := range notes {
		seen := make(map[string]struct{})
		if n.SymbolID != "" {
			symCount[n.SymbolID]++
			seen[n.SymbolID] = struct{}{}
		}
		for _, id := range n.AutoLinks {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			symCount[id]++
		}
		if n.FilePath != "" {
			fileCount[n.FilePath]++
		}
		for _, t := range n.Tags {
			tagCount[strings.ToLower(t)]++
		}
	}

	res.TopSymbols = topSymbolHits(symCount, opts.MaxSymbols, resolveNode)
	res.TopFiles = topCountHits(fileCount, opts.MaxFiles)
	res.TopTags = topCountHits(tagCount, opts.MaxTags)

	// Pinned notes — always surfaced.
	for _, n := range notes {
		if n.Pinned {
			res.PinnedNotes = append(res.PinnedNotes, toExcerpt(n, opts.ExcerptCap))
		}
	}
	// Decisions — notes tagged "decision".
	for _, n := range notes {
		if hasTag(n.Tags, "decision") {
			res.Decisions = append(res.Decisions, toExcerpt(n, opts.ExcerptCap))
		}
	}
	// Recent — newest-first, up to MaxRecent. Query already sorted DESC.
	for i := 0; i < len(notes) && i < opts.MaxRecent; i++ {
		res.Recent = append(res.Recent, toExcerpt(notes[i], opts.ExcerptCap))
	}
	if len(notes) > opts.MaxRecent {
		res.Truncated = true
	}

	res.Summary = renderDistillSummary(res)
	return res
}

// noteReferencesSymbol reports whether the note is attached to the
// given symbol either directly (SymbolID) or via auto-link.
func noteReferencesSymbol(e persistence.NoteEntry, sym string) bool {
	if e.SymbolID == sym {
		return true
	}
	return slices.Contains(e.AutoLinks, sym)
}

func hasTag(tags []string, needle string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, needle) {
			return true
		}
	}
	return false
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func normaliseTags(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		out = append(out, strings.ToLower(t))
	}
	return out
}

// newNoteID returns an 8-byte hex token. Crypto-strength because the
// IDs travel through MCP responses and can be referenced by other
// tool calls — predictability would let one session guess another's
// note IDs on a shared cache.
func newNoteID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to time-based to avoid panics on systems without /dev/urandom.
		return fmt.Sprintf("nt%016x", time.Now().UnixNano())
	}
	return "nt" + hex.EncodeToString(b[:])
}

func topSymbolHits(counts map[string]int, n int, resolve func(string) *graph.Node) []distillSymbolHit {
	if len(counts) == 0 {
		return nil
	}
	hits := make([]distillSymbolHit, 0, len(counts))
	for id, c := range counts {
		h := distillSymbolHit{ID: id, Count: c}
		if resolve != nil {
			if node := resolve(id); node != nil {
				h.Name = node.Name
				h.Kind = string(node.Kind)
				h.Path = node.FilePath
			}
		}
		hits = append(hits, h)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Count != hits[j].Count {
			return hits[i].Count > hits[j].Count
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > n {
		hits = hits[:n]
	}
	return hits
}

func topCountHits(counts map[string]int, n int) []distillCountHit {
	if len(counts) == 0 {
		return nil
	}
	hits := make([]distillCountHit, 0, len(counts))
	for v, c := range counts {
		hits = append(hits, distillCountHit{Value: v, Count: c})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Count != hits[j].Count {
			return hits[i].Count > hits[j].Count
		}
		return hits[i].Value < hits[j].Value
	})
	if len(hits) > n {
		hits = hits[:n]
	}
	return hits
}

func toExcerpt(n persistence.NoteEntry, cap int) distillExcerpt {
	body := n.Body
	if cap > 0 && len(body) > cap {
		body = body[:cap] + "…"
	}
	return distillExcerpt{
		ID:        n.ID,
		Body:      body,
		Tags:      append([]string{}, n.Tags...),
		Symbol:    n.SymbolID,
		UpdatedAt: n.UpdatedAt,
	}
}

// renderDistillSummary produces a small markdown blurb the agent
// can drop straight back into context. Deterministic — no LLM
// involvement here; rendering stays cheap and predictable.
func renderDistillSummary(res distillResult) string {
	if res.NoteCount == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Session digest: %d note(s) between %s and %s.",
		res.NoteCount,
		res.Window.From.UTC().Format(time.RFC3339),
		res.Window.To.UTC().Format(time.RFC3339),
	)
	if len(res.TopSymbols) > 0 {
		b.WriteString(" Top symbols: ")
		for i, s := range res.TopSymbols {
			if i > 0 {
				b.WriteString(", ")
			}
			name := s.Name
			if name == "" {
				name = s.ID
			}
			fmt.Fprintf(&b, "%s×%d", name, s.Count)
		}
		b.WriteString(".")
	}
	if len(res.TopTags) > 0 {
		b.WriteString(" Tags: ")
		for i, t := range res.TopTags {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s×%d", t.Value, t.Count)
		}
		b.WriteString(".")
	}
	if len(res.PinnedNotes) > 0 {
		fmt.Fprintf(&b, " %d pinned.", len(res.PinnedNotes))
	}
	if len(res.Decisions) > 0 {
		fmt.Fprintf(&b, " %d decision(s).", len(res.Decisions))
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Auto-linking
// ---------------------------------------------------------------------------

// autoLinkOptions tunes the body→symbol-ID extractor.
type autoLinkOptions struct {
	MaxLinks    int  // cap per note (default 20)
	MinTokenLen int  // ignore tokens shorter than this (default 4)
	HonorScope  bool // when true, only return symbols inside workspaceID
}

func defaultAutoLinkOptions() autoLinkOptions {
	return autoLinkOptions{
		MaxLinks: 20,
		// 3 chars is the minimum that still lets common short
		// identifiers (Bar, Err, Map, Buf, …) participate while
		// the stop-word list filters short English words.
		MinTokenLen: 3,
		HonorScope:  true,
	}
}

// autoLinkBody extracts referenced symbol IDs from the note body
// by:
//
//  1. Picking out anything that *looks* like an existing node ID
//     (`file/path.go::Symbol`, `file/path.go::Type.Method`) and
//     promoting it directly if `graph.GetNode` confirms it.
//  2. Tokenising the rest of the body into identifier-shaped words
//     (camelCase / snake_case / dotted), then resolving each via
//     `graph.FindNodesByName` and keeping the matches that pass
//     the optional workspace filter.
//
// The function never panics — a nil graph or empty body just
// returns no links. Results are deduplicated and capped.
func autoLinkBody(body string, g graph.Reader, workspaceID string, opts autoLinkOptions) []string {
	if g == nil || body == "" {
		return nil
	}
	if opts.MaxLinks <= 0 {
		opts.MaxLinks = 20
	}
	if opts.MinTokenLen <= 0 {
		opts.MinTokenLen = 3
	}

	seen := make(map[string]struct{})
	var out []string
	add := func(id string) bool {
		if id == "" {
			return false
		}
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) >= opts.MaxLinks
	}

	// (1) Direct ID matches — anything containing "::" is treated as
	// a candidate ID. Batch the lookup so even auto-linkers with many
	// candidates on long notes only pay one backend round-trip on
	// disk-backed stores.
	candidates := extractIDCandidates(body)
	candidateNodes := g.GetNodesByIDs(candidates)
	for _, candidate := range candidates {
		node := candidateNodes[candidate]
		if node == nil {
			continue
		}
		if opts.HonorScope && workspaceID != "" && node.WorkspaceID != workspaceID {
			continue
		}
		if add(node.ID) {
			return out
		}
	}

	// (2) Name-based resolution. Each unique token is queried once.
	tokens := tokeniseIdentifiers(body, opts.MinTokenLen)
	for _, tok := range tokens {
		// Plain-English single-word tokens like "memory", "tests",
		// "validation" routinely collide with field / constant
		// names elsewhere in the graph (`repoItem.memory`,
		// `Config.MCP`). Require a code-shape signal — uppercase
		// letter, underscore, dot, or `::` qualifier — before we
		// even try the name lookup. Without this the auto-linker
		// pulls in arbitrary nodes whose name happens to overlap
		// with a body word, and the surfaced memories look
		// unrelated to anything the note actually discusses.
		if !hasIdentifierSignal(tok) {
			continue
		}
		matches := g.FindNodesByName(tok)
		// One match means an unambiguous reference; anything more
		// is too noisy to auto-link without confirmation. The prior
		// threshold (up to 3) accepted false positives like the
		// body word "memory" pulling in three unrelated field
		// nodes.
		if len(matches) != 1 {
			continue
		}
		n := matches[0]
		if opts.HonorScope && workspaceID != "" && n.WorkspaceID != workspaceID {
			continue
		}
		if add(n.ID) {
			return out
		}
	}
	return out
}

// hasIdentifierSignal reports whether tok carries a positive
// "this is code, not English" marker. Pure-lowercase single-word
// tokens fail the test — they're indistinguishable from common
// vocabulary and overwhelm the auto-linker with accidental hits.
func hasIdentifierSignal(tok string) bool {
	hasUpper := false
	hasUnderscore := false
	hasDot := false
	for _, r := range tok {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r == '_':
			hasUnderscore = true
		case r == '.':
			hasDot = true
		}
	}
	return hasUpper || hasUnderscore || hasDot
}

// extractIDCandidates pulls every "<...>::<...>" run out of the body
// without allocating a regexp. Whitespace, commas, parens, and quotes
// terminate a candidate.
func extractIDCandidates(body string) []string {
	var out []string
	start := -1
	hasColon := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if isIDChar(c) {
			if start < 0 {
				start = i
			}
			if c == ':' && i+1 < len(body) && body[i+1] == ':' {
				hasColon = true
			}
			continue
		}
		if start >= 0 {
			if hasColon {
				out = append(out, body[start:i])
			}
			start = -1
			hasColon = false
		}
	}
	if start >= 0 && hasColon {
		out = append(out, body[start:])
	}
	return out
}

// isIDChar covers the characters that may appear in a Gortex node
// ID: letters, digits, underscore, hyphen, dot, slash, colon, hash,
// at-sign, plus the percent-encoded `@` we sometimes see in module
// IDs. Deliberately exclude common sentence punctuation (comma,
// semicolon, brackets, quotes).
func isIDChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z',
		c >= 'A' && c <= 'Z',
		c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '_', '-', '.', '/', ':', '#', '@', '+':
		return true
	}
	return false
}

// tokeniseIdentifiers walks the body, emitting identifier-shaped
// tokens of at least minLen runes. Returns each token once, in the
// order first seen — order matters because we cap auto-links at
// MaxLinks and earlier tokens win.
func tokeniseIdentifiers(body string, minLen int) []string {
	seen := make(map[string]struct{})
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		tok := cur.String()
		cur.Reset()
		if utf8RuneCount(tok) < minLen {
			return
		}
		if isStopWord(tok) {
			return
		}
		if _, dup := seen[tok]; dup {
			return
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	for _, r := range body {
		if isIdentRune(r) {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// isStopWord filters out common English words and trivial tokens
// that would otherwise blow up FindNodesByName lookups. The list
// is small on purpose — the goal is precision, not exhaustiveness.
func isStopWord(tok string) bool {
	if tok == "" {
		return true
	}
	low := strings.ToLower(tok)
	switch low {
	case "the", "and", "for", "with", "from", "this", "that", "these",
		"those", "into", "onto", "have", "has", "had", "but", "not",
		"are", "was", "were", "will", "would", "should", "could",
		"about", "after", "before", "when", "while", "then", "than",
		"because", "however", "also", "very", "much", "many", "some",
		"todo", "fixme", "note", "notes", "session":
		return true
	}
	return false
}
