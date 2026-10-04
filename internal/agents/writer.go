package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// writer.go centralises every write `gortex init` performs. Going
// through one helper lets us:
//
//  1. Make writes atomic — temp file + rename. Partial failures
//     can't leave a half-written MCP config that breaks the editor.
//  2. Respect ApplyOpts.DryRun uniformly. No adapter needs its own
//     "would this run?" branch.
//  3. Report what happened in a structured FileAction so `--json`
//     and the doctor subcommand speak the same vocabulary.
//  4. Power golden-fixture tests — the test harness points the
//     "root" at a temp dir, runs Apply, and diffs the written tree
//     against testdata/ golden files.

// WriteIfNotExists writes content to path when it doesn't exist.
// Used for static artifacts (slash-command markdown, Kiro steering,
// KI metadata) where merging isn't meaningful. When the file is
// already present we emit ActionSkip with Reason="exists" rather
// than silently overwriting.
//
// Under DryRun: no disk write. Returns ActionWouldCreate for a
// missing file, ActionSkip for an existing one.
//
// Directories are created as needed with 0o755.
func WriteIfNotExists(w io.Writer, path, content string, opts ApplyOpts) (FileAction, error) {
	if _, err := os.Stat(path); err == nil {
		logf(w, "[gortex init] skip %s (already exists)", path)
		return FileAction{Path: path, Action: ActionSkip, Reason: "exists"}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return FileAction{Path: path, Action: ActionSkip, Reason: err.Error()}, fmt.Errorf("stat %s: %w", path, err)
	}

	if opts.DryRun {
		return FileAction{Path: path, Action: ActionWouldCreate}, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := AtomicWriteFile(path, []byte(content), 0o644); err != nil {
		return FileAction{}, err
	}
	logf(w, "[gortex init] created %s", path)
	return FileAction{Path: path, Action: ActionCreate}, nil
}

// WriteOwnedFile writes content to path unconditionally, overwriting
// whatever is there. Meant for files Gortex owns end-to-end (e.g.
// generated community-routing files under .cursor/rules/,
// .continue/rules/, .clinerules/) so each `gortex init` run
// regenerates them from the current graph. Returns ActionCreate when
// the file was absent and ActionMerge when it already existed, so the
// summary line reads naturally.
//
// Under DryRun: no disk write. Returns ActionWould* mirroring the
// same existed/absent split.
func WriteOwnedFile(w io.Writer, path, content string, opts ApplyOpts) (FileAction, error) {
	existing, readErr := os.ReadFile(path)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, readErr)
	}

	// Skip when the target is already byte-identical — keeps
	// AssertIdempotent valid and avoids mtime bumps on no-op
	// re-runs.
	if existed && string(existing) == content {
		return FileAction{Path: path, Action: ActionSkip, Reason: "unchanged"}, nil
	}

	if opts.DryRun {
		action := ActionWouldCreate
		if existed {
			action = ActionWouldMerge
		}
		return FileAction{Path: path, Action: action}, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FileAction{}, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := AtomicWriteFile(path, []byte(content), 0o644); err != nil {
		return FileAction{}, err
	}
	verb := "wrote"
	if existed {
		verb = "regenerated"
	}
	logf(w, "[gortex init] %s %s", verb, path)
	action := ActionCreate
	if existed {
		action = ActionMerge
	}
	return FileAction{Path: path, Action: action}, nil
}

// AtomicWriteFile writes data to path via a temp file in the same
// directory followed by a rename. Guarantees that a concurrent reader
// either sees the old file or the fully-written new file — never a
// half-written state.
//
// The temp file carries a distinctive ".gortex.tmp-" infix, so a crash
// between CreateTemp and the rename leaves an identifiable
// "<name>.gortex.tmp-<rand>" orphan. Those orphans are not left to
// accumulate: every AtomicWriteFile first sweeps its target directory
// for stale ones via cleanStaleTempFiles.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Reap temp files an earlier write orphaned here before adding our
	// own: a crash (or kill) between CreateTemp and the rename below
	// strands the temp on disk, and the success path only ever consumes
	// THIS write's temp, so without an opportunistic sweep they pile up.
	cleanStaleTempFiles(dir)
	f, err := os.CreateTemp(dir, filepath.Base(path)+tempInfix+"*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpPath := f.Name()
	// Best-effort cleanup on failure. We deliberately ignore the
	// error: if the rename succeeds the file no longer exists, and
	// if something else goes wrong the user can remove the temp by
	// hand.
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := platform.ReplaceFile(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}

// tempInfix is the distinctive, Gortex-branded marker in every temp
// file AtomicWriteFile writes: os.CreateTemp expands the pattern
// "<base>.gortex.tmp-*" into "<base>.gortex.tmp-<random>". Sharing one
// const between the writer and the reaper keeps the sweep from ever
// drifting out of sync with the names it is meant to match.
const tempInfix = ".gortex.tmp-"

// staleTempAge is how long an orphaned temp must sit untouched before a
// later write reaps it. It only has to exceed the lifetime of one
// in-flight AtomicWriteFile — a data write plus platform.ReplaceFile's bounded
// retry budget — so an hour is orders of magnitude of headroom and guarantees
// a concurrent write's fresh temp is never mistaken for debris.
const staleTempAge = time.Hour

// cleanStaleTempFiles best-effort removes temp files an earlier
// AtomicWriteFile orphaned in dir when its process died between
// CreateTemp and the rename. Only files that carry our own tempInfix
// AND have gone untouched for longer than staleTempAge are removed, so
// a temp another writer is still filling in (or about to rename) is
// never disturbed. Every error is swallowed: this is directory hygiene,
// not a load-bearing step — a write must never fail because a leftover
// could not be reaped, and a dir we cannot read simply has nothing to
// sweep from this caller's point of view.
func cleanStaleTempFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleTempAge)
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), tempInfix) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// MergeJSON reads path (if present), parses it as a JSON object,
// passes the parsed map to mutate, and writes the result back
// atomically when mutate reports a change. A nil or malformed file
// is treated as empty — a backup is written alongside the original
// before we overwrite garbage, so a user with a broken config doesn't
// lose their edits.
//
// mutate returns:
//   - changed=true if the map was modified and should be written
//   - changed=false if no change is needed (idempotent re-run); we
//     still return a FileAction describing the skip for --json
//
// Keys is collected from the top-level map keys after mutation —
// useful for the --json report but not semantically load-bearing.
func MergeJSON(w io.Writer, path string, mutate func(root map[string]any, existed bool) (changed bool, err error), opts ApplyOpts) (FileAction, error) {
	existed := false
	root := make(map[string]any)
	var backupPath string

	var backupData []byte
	if data, err := os.ReadFile(path); err == nil {
		existed = true
		switch {
		case len(strings.TrimSpace(string(data))) == 0:
			// An empty (or whitespace-only) file is an empty object, not
			// malformed — no backup, nothing to preserve.
		default:
			// `.jsonc` / `.json5` configs (e.g. OpenCode's
			// opencode.jsonc) may carry comments and trailing commas
			// that encoding/json rejects. Sanitize those before the
			// parse so a commented config merges instead of being
			// treated as malformed and clobbered. Comments are not
			// round-tripped through the re-marshal — same policy as
			// MergeTOML.
			parse := data
			if isJSONCPath(path) {
				parse = stripJSONComments(data)
			}
			if err := json.Unmarshal(parse, &root); err != nil {
				// Don't silently overwrite the user's file even if it's
				// malformed — keep a backup for recovery. The backup is
				// written only on the real write path below, so a DryRun
				// never touches disk.
				backupPath, backupData = path+".bak", data
				root = make(map[string]any)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}

	changed, err := mutate(root, existed)
	if err != nil {
		return FileAction{}, err
	}
	if !changed {
		return FileAction{Path: path, Action: ActionSkip, Reason: "already-configured"}, nil
	}

	keys := sortedMapKeys(root)

	if opts.DryRun {
		action := ActionWouldCreate
		if existed {
			action = ActionWouldMerge
		}
		return FileAction{Path: path, Action: action, Keys: keys}, nil
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return FileAction{}, fmt.Errorf("marshal %s: %w", path, err)
	}
	if backupPath != "" {
		// Best-effort backup of the malformed original, just before we
		// overwrite it (never under DryRun — that returned above).
		_ = os.WriteFile(backupPath, backupData, 0o644)
	}
	if err := AtomicWriteFile(path, out, 0o644); err != nil {
		return FileAction{}, err
	}

	action := ActionCreate
	if existed {
		action = ActionMerge
	}
	if backupPath != "" {
		logf(w, "[gortex init] %s was malformed; backup saved to %s", path, backupPath)
	}
	logf(w, "[gortex init] %s %s", actionVerb(action), path)
	return FileAction{Path: path, Action: action, Keys: keys}, nil
}

// isJSONCPath reports whether path uses a JSON-with-comments extension
// (`.jsonc` / `.json5`) whose contents may need sanitising before they
// can be handed to encoding/json.
func isJSONCPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonc", ".json5":
		return true
	default:
		return false
	}
}

// StripJSONComments is the exported form, for an adapter that has to
// READ a host config MergeJSON is not writing — an uninstall preview
// deciding whether a stanza is actually present, say. Reaching for
// encoding/json directly on a host that permits JSONC silently reports
// every commented config as unparseable.
func StripJSONComments(b []byte) []byte { return stripJSONComments(b) }

// stripJSONComments rewrites JSONC / JSON5-style input into strict JSON
// that encoding/json can parse: it drops `//` line comments, `/* */`
// block comments, and trailing commas before `}` / `]`. String literals
// (and their escape sequences) are copied through untouched, so a `//`
// or comma inside a quoted value is preserved. The result is only used
// for parsing — the merged map is re-marshalled fresh, so the original
// comments and formatting are not carried over (matching MergeTOML).
func stripJSONComments(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString, escaped := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			// Line comment: skip to (but keep) the newline.
			for i+1 < len(b) && b[i+1] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			// Block comment: skip through the closing */.
			i += 2
			for i+1 < len(b) && (b[i] != '*' || b[i+1] != '/') {
				i++
			}
			i++ // step onto '/', loop's i++ moves past it
		default:
			out = append(out, c)
		}
	}
	return stripTrailingCommas(out)
}

// stripTrailingCommas removes a comma that is followed (ignoring
// whitespace) by a `}` or `]` — JSONC / JSON5 permit it, strict JSON
// does not. Commas inside string literals are left alone.
func stripTrailingCommas(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString, escaped := false, false
	for i := range len(b) {
		c := b[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(b) {
				switch b[j] {
				case ' ', '\t', '\n', '\r':
					j++
					continue
				}
				break
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				continue // drop the trailing comma
			}
		}
		out = append(out, c)
	}
	return out
}

// actionVerb renders an ActionKind for human-readable log lines.
// Kept distinct from the on-the-wire string so we can tweak messaging
// without breaking JSON consumers.
func actionVerb(a ActionKind) string {
	switch a {
	case ActionCreate:
		return "created"
	case ActionMerge:
		return "merged into"
	case ActionSkip:
		return "skipped"
	case ActionWouldCreate:
		return "would create"
	case ActionWouldMerge:
		return "would merge into"
	}
	return string(a)
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// logf writes a newline-terminated message when w is non-nil. Shared
// helper so adapters don't each need to guard for a nil stderr in
// tests.
func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}
