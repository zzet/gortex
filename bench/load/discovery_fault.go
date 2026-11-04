package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type discoveryOptions struct {
	StallFirst        bool
	Prepare           string
	Resume            string
	RuntimeRoot       string
	ExistingWorktrees int
	ExistingNested    int
}

func bindDiscoveryFlags(f *flag.FlagSet, o *discoveryOptions) {
	f.StringVar(&o.Prepare, "discovery-prepare", "", "worktree-discovery: prepare owned roots and write a descriptor without launching a daemon")
	f.StringVar(&o.Resume, "discovery-resume", "", "worktree-discovery: consume exact owned upgraded-store lineage at unchanged prepared roots")
	f.StringVar(&o.RuntimeRoot, "discovery-runtime-root", "", "discovery resume: fresh canonical runtime directory directly under scratch; prepared repository roots stay fixed")
	f.IntVar(&o.ExistingNested, "discovery-existing-nested", 0, "discovery preparation: existing linked checkouts placed inside the primary repository")
	f.IntVar(&o.ExistingWorktrees, "discovery-existing-worktrees", 0, "discovery preparation: existing linked checkouts of the first family")
	f.BoolVar(&o.StallFirst, "discovery-stall-first", false, "worktree-discovery: hold Git stdout for the first new owned checkout, ignoring TERM with a 60-second child bound")
}
func (o discoveryOptions) validate(scenario string, count int) error {
	if o.RuntimeRoot != "" && o.Resume == "" {
		return errors.New("discovery runtime root requires discovery resume")
	}
	if o.ExistingNested < 0 || o.ExistingNested > o.ExistingWorktrees {
		return errors.New("discovery existing nested count must fit the existing topology")
	}
	if o.ExistingWorktrees < 0 {
		return errors.New("discovery existing-worktree count must be nonnegative")
	}
	if o.Prepare != "" || o.Resume != "" || o.ExistingWorktrees != 0 || o.ExistingNested != 0 {
		if scenario != checkoutDiscoveryScenario || o.Prepare != "" && o.Resume != "" || o.ExistingWorktrees != 0 && o.Prepare == "" {
			return errors.New("discovery prepare/resume flags require that scenario, exclusive modes and preparation for existing topology")
		}
		if runtime.GOOS == "windows" {
			return errors.New("owned discovery seed resume currently requires Unix owner checks")
		}
	}
	if !o.StallFirst {
		return nil
	}
	if scenario != checkoutDiscoveryScenario || count < 1 {
		return errors.New("--discovery-stall-first requires worktree-discovery and at least one new worktree")
	}
	if runtime.GOOS == "windows" {
		return errors.New("discovery Git stall requires Unix process ownership")
	}
	return nil
}

func discoveryShellLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// No shell runs while preparing this adapter. Only the owned daemon receives
// its PATH; worktree creation and all harness Git retain the real executable.
func (p *privateRun) installDiscoveryFault() (func() error, map[string]string, error) {
	if runtime.GOOS == "windows" {
		return nil, nil, errors.New("discovery Git stall requires Unix")
	}
	root, err := canonicalPath(p.Root)
	if err != nil || root != p.Root {
		return nil, nil, errors.New("discovery fault requires its canonical private root")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		return nil, nil, err
	}
	realGit, err = filepath.Abs(realGit)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(p.Root, "discovery-git-fault")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, nil, err
	}
	target := filepath.Join(p.Root, "repos", "discovery-linked-0-0")
	release, events := filepath.Join(dir, "release"), filepath.Join(dir, "events.tsv")
	script := fmt.Sprintf(`#!/bin/sh
cwd=$(pwd -P)
want_cwd=0
operation=
for arg in "$@"; do
 if [ "$want_cwd" = 1 ]; then cwd=$arg; want_cwd=0; continue; fi
 case "$arg" in -C) want_cwd=1;; rev-parse|worktree) operation=$arg;; esac
done
case "$cwd" in %s|%s/*)
 if [ -n "$operation" ]; then
  ready=%s.ready.$$
  (trap '' TERM HUP INT
   : > "$ready"
   count=0
   while [ ! -f %s ] && [ "$count" -lt 60 ]; do sleep 1; count=$((count+1)); done
  ) &
  child=$!
  while [ ! -f "$ready" ] && kill -0 "$child" 2>/dev/null; do sleep 0.01; done
  printf '%%s\t%%s\t%%s\n' "$child" "$cwd" "$operation" >> %s
  wait "$child"
  exit 124
 fi;;
esac
exec %s "$@"
`, discoveryShellLiteral(target), discoveryShellLiteral(target), discoveryShellLiteral(events), discoveryShellLiteral(release), discoveryShellLiteral(events), discoveryShellLiteral(realGit))
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		return nil, nil, err
	}
	env := append([]string(nil), p.Env...)
	found := false
	for i, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			env[i] = "PATH=" + dir + string(os.PathListSeparator) + strings.TrimPrefix(entry, "PATH=")
			found = true
		}
	}
	if !found {
		env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	p.daemonEnv = env
	return func() error { return os.WriteFile(release, []byte("release owned Git descendants\n"), 0o600) }, map[string]string{"discovery_git_shim": shim, "discovery_git_events": events, "discovery_git_release": release}, nil
}
