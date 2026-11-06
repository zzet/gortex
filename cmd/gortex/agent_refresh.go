package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/config"
)

// agent_refresh.go brings the agent files an older binary generated up to
// date the first time a new binary runs, whichever way it was installed
// (#1052). The daemon start runs it, and so does the first CLI command after
// a binary swap, for machines where the daemon keeps running. The decision
// of what may be rewritten lives in agents.RefreshOwnedFiles; this file only
// gates it on a version change, serialises concurrent runs and reports.

// agentRefreshManifestPath is a package var so tests can redirect it.
var agentRefreshManifestPath = agents.GeneratedManifestPath

// agentRefreshReport receives the outcome of one refresh pass.
type agentRefreshReport struct {
	entry func(agents.RefreshEntry)
	fail  func(error)
	// quietWhenOff drops the stale-config reports while auto_refresh is
	// off. That pass never stamps the version, so it runs on every CLI
	// command, and repeating the same notice there helps no one; doctor
	// and the daemon log still carry it.
	quietWhenOff bool
}

// ownedFileListers returns every registered adapter that owns whole files.
func ownedFileListers() []agents.NamedLister {
	var out []agents.NamedLister
	for _, a := range buildRegistry().All() {
		if l, ok := a.(agents.OwnedFileLister); ok {
			out = append(out, agents.NamedLister{Name: a.Name(), Lister: l})
		}
	}
	return out
}

// staleConfigEntries reports the merged configs an adapter manages that are
// out of date. They are never rewritten, so they are reported as stale on
// every pass, with whatever the user has to do beyond reinstalling.
func staleConfigEntries(env agents.Env, from string) []agents.RefreshEntry {
	var out []agents.RefreshEntry
	for _, a := range buildRegistry().All() {
		r, ok := a.(agents.StaleConfigReporter)
		if !ok {
			continue
		}
		for _, c := range r.StaleConfigs(env) {
			out = append(out, agents.RefreshEntry{Agent: a.Name(), Path: c.Path, Status: agents.RefreshStale,
				FromVersion: from, ToVersion: env.BinaryVersion, Note: c.Note})
		}
	}
	return out
}

// agentRefreshEnv is the environment the owned files are rendered in. The
// posture is not part of it: each file carries its own and Render reads it.
func agentRefreshEnv() (agents.Env, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return agents.Env{}, fmt.Errorf("resolve home: %w", errors.Join(err, errors.New("empty home")))
	}
	return agents.Env{
		BinaryVersion: canonicalVersion(),
		Home:          home,
		HookCommand:   claudecode.ResolveHookCommand(io.Discard),
		Mode:          agents.ModeGlobal,
		Stderr:        io.Discard,
	}, nil
}

// refreshAgentFilesIfVersionChanged runs one refresh pass when the manifest
// was written by a different release. It is cheap when nothing changed: one
// small file read. A concurrent pass (daemon start racing a CLI command or
// an install) holds the lock, and this one simply leaves the work to it.
func refreshAgentFilesIfVersionChanged(r agentRefreshReport) {
	path := agentRefreshManifestPath()
	version := canonicalVersion()
	prev, err := loadAgentManifest(path)
	if err != nil {
		r.fail(err)
		return
	}
	if prev != nil && prev.Version == version {
		return
	}
	env, err := agentRefreshEnv()
	if err != nil {
		r.fail(err)
		return
	}
	from := ""
	if prev != nil {
		from = prev.Version
	}
	stale := staleConfigEntries(env, from)
	if prev == nil && len(stale) == 0 && !ownedFilesPresent(env) {
		return // nothing generated on this machine: leave no trace either
	}
	unlock, ok := lockAgentFiles(path, false)
	if !ok {
		return
	}
	defer unlock()
	if prev, err = loadAgentManifest(path); err != nil {
		r.fail(err)
		return
	} else if prev != nil && prev.Version == version {
		return
	}
	apply := true
	if gc, err := config.LoadGlobal(); err == nil && gc != nil {
		apply = gc.Agents.AutoRefreshEnabled()
	}
	entries, next, err := agents.RefreshOwnedFiles(ownedFileListers(), env, prev, apply)
	if !apply && r.quietWhenOff {
		stale = nil
	}
	for _, e := range append(entries, stale...) {
		r.entry(e)
	}
	if err != nil {
		r.fail(err)
		return // keep the old manifest so the next run retries
	}
	if !apply {
		return // unstamped, so turning auto_refresh back on refreshes at once
	}
	next.AppliedAt = time.Now().UTC()
	if err := agents.SaveGeneratedManifest(path, next); err != nil {
		r.fail(err)
	}
}

// loadAgentManifest reads the manifest, treating a corrupt one as absent:
// the shipped-render rule still guards every file, and the next successful
// pass replaces it.
func loadAgentManifest(path string) (*agents.GeneratedManifest, error) {
	m, err := agents.LoadGeneratedManifest(path)
	if errors.Is(err, agents.ErrCorruptManifest) {
		return nil, nil
	}
	return m, err
}

// ownedFilesPresent reports whether any owned agent file exists, so a
// machine that never installed one pays for no lock and gets no manifest.
func ownedFilesPresent(env agents.Env) bool {
	for _, nl := range ownedFileListers() {
		for _, f := range nl.Lister.OwnedFiles(env) {
			if _, err := os.Lstat(f.Path); err == nil {
				return true
			}
		}
	}
	return false
}

// lockAgentFiles takes the lock that serialises refresh passes and installs.
// A refresh only tries (another pass is doing the work); an install waits,
// so a refresh that read the old copy cannot overwrite what install wrote.
func lockAgentFiles(path string, wait bool) (unlock func(), ok bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false
	}
	lock := flock.New(path + ".lock")
	var locked bool
	var err error
	if wait {
		err = lock.Lock()
		locked = err == nil
	} else {
		locked, err = lock.TryLock()
	}
	if err != nil || !locked {
		_ = lock.Close()
		return nil, false
	}
	return func() { _ = lock.Unlock() }, true
}

// recordAgentFiles records the hashes of the owned files an install just
// wrote, so the next binary can tell them from user edits. It never moves
// the recorded version: a partial install (one --agents, --no-hooks) leaves
// other owned files as they were, and only a refresh pass may decide they
// are current. The next command of this binary runs that pass.
func recordAgentFiles(env agents.Env) error {
	path := agentRefreshManifestPath()
	prev, err := loadAgentManifest(path)
	if err != nil {
		return err
	}
	_, next, err := agents.RefreshOwnedFiles(ownedFileListers(), env, prev, false)
	if err != nil {
		return err
	}
	if prev == nil && len(next.Files) == 0 {
		return nil
	}
	next.Version = ""
	if prev != nil {
		next.Version = prev.Version
	}
	next.AppliedAt = time.Now().UTC()
	return agents.SaveGeneratedManifest(path, next)
}

// agentRefreshRefreshCommand is what a user runs to bring a file back to
// what Gortex generates, discarding their edits.
func agentRefreshRefreshCommand(agent string) string {
	return "gortex install --agents=" + agent
}

// cliAgentRefreshSkips are commands that must not pay for, or race with, the
// refresh: the per-tool-call hook, the stdio proxy, the daemon (which runs
// its own pass at start) and the commands that write the files themselves.
var cliAgentRefreshSkips = map[string]bool{
	"hook": true, "githook": true, "mcp": true, "daemon": true, "install": true, "init": true,
	"uninstall": true, "upgrade": true, "__complete": true, "completion": true,
}

// refreshAgentFilesFromCLI is the CLI half of the version gate.
func refreshAgentFilesFromCLI(cmd *cobra.Command) {
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		if cliAgentRefreshSkips[c.Name()] {
			return
		}
	}
	stderr := cmd.ErrOrStderr()
	refreshAgentFilesIfVersionChanged(agentRefreshReport{
		entry: func(e agents.RefreshEntry) {
			switch e.Status {
			case agents.RefreshRewritten:
				fmt.Fprintf(stderr, "[gortex] refreshed %s (%s -> %s)\n", e.Path, displayVersion(e.FromVersion), e.ToVersion)
			case agents.RefreshModified, agents.RefreshUnrecognized:
				fmt.Fprintf(stderr, "[gortex] kept %s: it is not what Gortex generated; run `%s` to replace it\n", e.Path, agentRefreshRefreshCommand(e.Agent))
			case agents.RefreshStale:
				if e.Note != "" { // a merged config, never rewritten
					fmt.Fprintf(stderr, "[gortex] %s is outdated and was not rewritten; run `%s`. %s\n", e.Path, agentRefreshRefreshCommand(e.Agent), e.Note)
				}
			}
		},
		fail: func(err error) {
			fmt.Fprintf(stderr, "[gortex] could not refresh agent files: %v\n", err)
		},
		quietWhenOff: true,
	})
}

func displayVersion(v string) string {
	if v == "" {
		return "unrecorded"
	}
	return v
}

// doctorGeneratedFile is one owned agent file that is not current, as
// `gortex doctor` reports it.
type doctorGeneratedFile struct {
	agents.RefreshEntry
	Refresh string `json:"refresh"`
}

// doctorGeneratedFiles judges the owned agent files without writing, so
// doctor reports stale and edited files whether or not auto-refresh is on.
func doctorGeneratedFiles() []doctorGeneratedFile {
	env, err := agentRefreshEnv()
	if err != nil {
		return nil
	}
	prev, _ := loadAgentManifest(agentRefreshManifestPath())
	entries, _, _ := agents.RefreshOwnedFiles(ownedFileListers(), env, prev, false)
	from := ""
	if prev != nil {
		from = prev.Version
	}
	var out []doctorGeneratedFile
	for _, e := range append(entries, staleConfigEntries(env, from)...) {
		if e.Status == agents.RefreshCurrent {
			continue
		}
		e.Path = doctorPath(e.Path)
		out = append(out, doctorGeneratedFile{RefreshEntry: e, Refresh: agentRefreshRefreshCommand(e.Agent)})
	}
	return out
}

func printDoctorGeneratedFiles(w io.Writer, files []doctorGeneratedFile) {
	if len(files) == 0 {
		return
	}
	fmt.Fprintln(w, "\nGenerated agent files")
	for _, f := range files {
		why := map[agents.RefreshStatus]string{
			agents.RefreshStale:        staleReason(),
			agents.RefreshModified:     "edited since Gortex wrote it",
			agents.RefreshUnrecognized: "not a file Gortex generated",
		}[f.Status]
		if f.Note != "" {
			why = "outdated, not rewritten automatically"
		}
		fmt.Fprintf(w, "  %s %s: %s — run `%s` to replace it\n", glyphWarn, f.Path, why, f.Refresh)
		if f.Note != "" {
			fmt.Fprintf(w, "      %s\n", f.Note)
		}
	}
}

func staleReason() string {
	if gc, err := config.LoadGlobal(); err == nil && gc != nil && !gc.Agents.AutoRefreshEnabled() {
		return "outdated (agents.auto_refresh is off)"
	}
	return "outdated"
}
