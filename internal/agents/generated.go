package agents

// generated.go keeps files Gortex wholly owns current after the binary is
// replaced by something other than `gortex upgrade --run` (brew, Scoop, the
// install script, `go install`). Those paths never re-run `gortex install`,
// so a generated file stays as the old binary wrote it; for the OpenCode
// bridge that meant a plugin OpenCode 2 refused to load, with every hook off
// and nothing left running to say so (#830).
//
// Ownership is proven, never assumed. A sidecar manifest records the hash of
// each file as Gortex last wrote it, the same way instruction profiles keep
// .generated.json beside them: the files' own bytes stay untouched, which
// matters for frontmatter, JSON and @-included text. A file is refreshed only
// when it is still exactly what Gortex wrote (the recorded hash), or, before
// any manifest existed, when its adapter recognises it as a render some
// release shipped. Anything else is the user's and is only reported. A file
// that is absent is never created: absence is how `--no-hooks` and a removed
// skill look on disk.
//
// Only whole files an adapter fully owns take part. Merged configs (MCP
// stanzas, settings.json hooks, config.toml) are out of scope: the refresh
// never runs an adapter's Apply, so it cannot reach them.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// GeneratedManifestFileName is the sidecar in the Gortex data dir that
// records which release last applied the owned agent files, and their hashes.
const GeneratedManifestFileName = "agents-applied.json"

// GeneratedManifestPath is where the manifest lives on this machine.
func GeneratedManifestPath() string {
	return filepath.Join(platform.DataDir(), GeneratedManifestFileName)
}

// OwnedFile is one file an adapter writes and owns end to end.
type OwnedFile struct {
	Path string
	// Render returns the bytes the running binary writes at Path, carrying
	// over the settings the installed copy holds (hook posture, enforcement),
	// never install defaults. existing is the installed copy. recognized
	// reports whether existing is a render some Gortex release wrote, this
	// one included; it is what vouches for a copy the manifest has no record
	// of. A copy whose settings cannot be read back returns itself, not
	// recognized, and is left alone.
	Render func(existing []byte) (content string, recognized bool)
}

// OwnedFileLister is implemented by adapters that own whole files in global
// mode. Optional: an adapter without it takes no part in the refresh.
type OwnedFileLister interface {
	OwnedFiles(env Env) []OwnedFile
}

// GeneratedManifest is the on-disk record of the last refresh or install.
type GeneratedManifest struct {
	Version   string            `json:"version"`
	AppliedAt time.Time         `json:"applied_at"`
	Files     map[string]string `json:"files"` // path -> sha256 of the bytes Gortex wrote
}

// ErrCorruptManifest marks a manifest that exists but cannot be parsed.
// Callers treat it as absent: the strict shipped-render rule still guards
// every file, and the next successful pass writes a clean manifest.
var ErrCorruptManifest = errors.New("corrupt generated-files manifest")

// LoadGeneratedManifest reads the manifest at path. A missing file is
// (nil, nil): every install predating the manifest looks like that.
func LoadGeneratedManifest(path string) (*GeneratedManifest, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m GeneratedManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w %s: %v", ErrCorruptManifest, path, err)
	}
	return &m, nil
}

// SaveGeneratedManifest writes m atomically.
func SaveGeneratedManifest(path string, m *GeneratedManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return AtomicWriteFile(path, append(data, '\n'), 0o644)
}

// RefreshStatus is the verdict on one owned file.
type RefreshStatus string

const (
	RefreshCurrent      RefreshStatus = "current"      // already what this binary writes
	RefreshRewritten    RefreshStatus = "refreshed"    // rewritten in place
	RefreshStale        RefreshStatus = "stale"        // ours and outdated, not rewritten (apply off)
	RefreshModified     RefreshStatus = "modified"     // differs from what Gortex recorded writing
	RefreshUnrecognized RefreshStatus = "unrecognized" // no record, and not a render any release shipped
)

// RefreshEntry reports one owned file.
type RefreshEntry struct {
	Agent       string        `json:"agent"`
	Path        string        `json:"path"`
	Status      RefreshStatus `json:"status"`
	FromVersion string        `json:"from_version,omitempty"`
	ToVersion   string        `json:"to_version,omitempty"`
}

// NamedLister pairs an adapter name with its owned files, for reporting and
// for the refresh command a stale entry points at.
type NamedLister struct {
	Name   string
	Lister OwnedFileLister
}

// RefreshOwnedFiles judges every present owned file and, when apply is set,
// rewrites the ones that are provably Gortex's and outdated. It returns the
// per-file verdicts and the manifest describing disk afterwards; prev may be
// nil. A read or write error stops nothing else but is joined into err, and
// a caller should then keep the old manifest so the next run tries again.
func RefreshOwnedFiles(listers []NamedLister, env Env, prev *GeneratedManifest, apply bool) ([]RefreshEntry, *GeneratedManifest, error) {
	from := ""
	recorded := map[string]string{}
	if prev != nil {
		from = prev.Version
		for p, h := range prev.Files {
			recorded[p] = h
		}
	}
	next := &GeneratedManifest{Version: env.BinaryVersion, Files: map[string]string{}}
	var entries []RefreshEntry
	var errs []error
	for _, nl := range listers {
		for _, f := range nl.Lister.OwnedFiles(env) {
			info, err := os.Lstat(f.Path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			entry := RefreshEntry{Agent: nl.Name, Path: f.Path, FromVersion: from, ToVersion: env.BinaryVersion}
			if !info.Mode().IsRegular() {
				// A symlink (dotfiles) or anything else Gortex never writes
				// itself: replacing it would break the user's setup.
				entry.Status = RefreshUnrecognized
				entries = append(entries, entry)
				continue
			}
			existing, err := os.ReadFile(f.Path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			hash := GeneratedHash(existing)
			want, inManifest := recorded[f.Path]
			content, recognized := f.Render(existing)
			switch {
			case inManifest && want != hash:
				entry.Status = RefreshModified
				next.Files[f.Path] = want // keep flagging it until the user decides
			case !inManifest && !recognized:
				// Checked before "current": a copy the adapter cannot read
				// back renders as itself, and must not be recorded as ours.
				entry.Status = RefreshUnrecognized
			case string(existing) == content:
				entry.Status = RefreshCurrent
				next.Files[f.Path] = hash
			case !apply:
				entry.Status = RefreshStale
				if inManifest {
					next.Files[f.Path] = want
				}
			default:
				if err := AtomicWriteFile(f.Path, []byte(content), 0o644); err != nil {
					errs = append(errs, err)
					continue
				}
				entry.Status = RefreshRewritten
				next.Files[f.Path] = GeneratedHash([]byte(content))
			}
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, next, errors.Join(errs...)
}

// GeneratedHash fingerprints file bytes for the manifest.
func GeneratedHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
