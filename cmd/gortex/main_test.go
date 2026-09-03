package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/testenv"
)

// TestMain gives the whole package a sandboxed per-user environment, and
// then proves the developer's real one came through the run untouched.
//
// Both halves earn their keep. Individual tests do isolate themselves,
// but the isolation was easy to get wrong in a way no CI job could see:
// t.Setenv("HOME", dir) does not move os.UserHomeDir on Windows, so
// tests that looked isolated on the linux and macos matrix were
// appending their temp directories to the real config.yaml as tracked
// repos, merging hooks into the real ~/.codex/config.toml, and pruning
// shipped skills out of the real ~/.claude/skills. The package-wide
// sandbox makes forgetting harmless; the fingerprint check catches
// anything that reaches the real home by a route the sandbox misses.
func TestMain(m *testing.M) {
	before := map[string]string{}
	for _, p := range realUserStatePaths() {
		before[p] = fingerprint(p)
	}

	restore, err := testenv.SandboxProcess()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmd/gortex: cannot sandbox the test environment: %v\n", err)
		os.Exit(1)
	}
	queryLogDisabled, hadQueryLogDisabled := os.LookupEnv("GORTEX_QUERY_LOG_DISABLE")
	if err := os.Setenv("GORTEX_QUERY_LOG_DISABLE", "1"); err != nil {
		fmt.Fprintf(os.Stderr, "cmd/gortex: cannot disable query telemetry in tests: %v\n", err)
		restore()
		os.Exit(1)
	}

	code := m.Run()
	if hadQueryLogDisabled {
		_ = os.Setenv("GORTEX_QUERY_LOG_DISABLE", queryLogDisabled)
	} else {
		_ = os.Unsetenv("GORTEX_QUERY_LOG_DISABLE")
	}
	restore()

	if report := diffUserState(before); report != "" {
		fmt.Fprint(os.Stderr, report)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// realUserStatePaths lists the per-user files this package has been
// caught writing through to, resolved against the ambient environment
// before the sandbox is installed. The paths are captured rather than
// re-resolved afterwards, so the check does not depend on the
// environment having been restored correctly.
func realUserStatePaths() []string {
	paths := []string{
		config.DefaultGlobalConfigPath(),
		filepath.Join(platform.ConfigDir(), "servers.toml"),
		filepath.Join(platform.CacheDir(), "query-log.jsonl"),
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths,
			filepath.Join(home, ".codex", "config.toml"),
			filepath.Join(home, ".codex", "AGENTS.md"),
			filepath.Join(home, ".claude", "settings.json"),
			filepath.Join(home, ".claude", "CLAUDE.md"),
			filepath.Join(home, ".claude", "skills"),
		)
	}
	return paths
}

// diffUserState re-fingerprints every captured path and describes what
// moved, or returns "" when the real state is intact.
func diffUserState(before map[string]string) string {
	var changed []string
	for p, was := range before {
		if now := fingerprint(p); now != was {
			changed = append(changed, fmt.Sprintf("  %s (%s -> %s)", p, was, now))
		}
	}
	if len(changed) == 0 {
		return ""
	}
	sort.Strings(changed)
	return fmt.Sprintf(
		"\ncmd/gortex: a test wrote through to the developer's real per-user state:\n%s\n\n"+
			"Isolate the test with internal/testenv (Sandbox / SandboxAt / UnifiedHome / SetHome)\n"+
			"instead of t.Setenv(\"HOME\", ...), which does not move os.UserHomeDir on Windows.\n"+
			"If a gortex daemon was running and tracked a repository during this run, that is a\n"+
			"false positive — re-run with the daemon stopped to confirm.\n",
		strings.Join(changed, "\n"))
}

// fingerprint reduces a path to a value that changes whenever its
// contents do. A directory is fingerprinted by its entry names, which is
// enough to catch a prune of ~/.claude/skills without walking a tree of
// unknown size.
func fingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "absent"
	}
	if info.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return "unreadable"
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		return shortHash([]byte(strings.Join(names, "\n")))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	return shortHash(data)
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
