package githooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// initRepo creates a fresh git repo at tmp and returns the root path.
// core.hooksPath is pinned to a repo-local "hooks" dir so HookPathFor
// (which reads merged local+global git config) can never resolve to a
// machine-global hooks dir — without this, running the suite on a host
// with a global core.hooksPath makes every install/uninstall test
// write to the real global hooks.
func initRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Tester"},
		{"config", "core.hooksPath", hooksDir},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return tmp
}

func TestInstallHookPostCommit_FreshFile(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true, RegenWiki: true, Binary: "gortex"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"#!/bin/sh",
		MarkerBegin,
		MarkerEnd,
		"gortex export --format mermaid",
		"gortex wiki",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat hook: %v", err)
	}
	// NTFS has no exec bit — every file there reports 0666 and os.Chmod
	// only toggles the read-only attribute, so the mode says nothing about
	// whether Git will run the hook. Git for Windows runs hooks through its
	// bundled sh regardless of permissions.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode&0o100 == 0 {
			t.Errorf("hook not executable: mode = %v", mode)
		}
	}
}

func TestInstallHookPostCommit_Idempotent(t *testing.T) {
	repo := initRepo(t)
	for i := range 3 {
		if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if c := strings.Count(got, MarkerBegin); c != 1 {
		t.Errorf("expected one MarkerBegin, got %d", c)
	}
	if c := strings.Count(got, MarkerEnd); c != 1 {
		t.Errorf("expected one MarkerEnd, got %d", c)
	}
}

func TestInstallHookPostCommit_PreservesUserContent(t *testing.T) {
	repo := initRepo(t)
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	preexisting := `#!/bin/sh
# my custom hook
echo "hello from user hook"
`
	if err := os.WriteFile(hookPath, []byte(preexisting), 0o755); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `echo "hello from user hook"`) {
		t.Errorf("install should preserve user content; got:\n%s", got)
	}
	if !strings.Contains(got, MarkerBegin) {
		t.Errorf("install should add marker block; got:\n%s", got)
	}
}

func TestUninstallHookPostCommit_RemovesBlock(t *testing.T) {
	repo := initRepo(t)
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	preexisting := `#!/bin/sh
# my custom hook
echo "hello"
`
	if err := os.WriteFile(hookPath, []byte(preexisting), 0o755); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenWiki: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !removed {
		t.Error("Uninstall should report removed=true")
	}
	if path != hookPath {
		t.Errorf("Uninstall path mismatch: %q vs %q", path, hookPath)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook after uninstall: %v", err)
	}
	got := string(body)
	if strings.Contains(got, MarkerBegin) || strings.Contains(got, MarkerEnd) {
		t.Errorf("Uninstall should remove markers; got:\n%s", got)
	}
	if !strings.Contains(got, `echo "hello"`) {
		t.Errorf("Uninstall should preserve user content; got:\n%s", got)
	}
}

func TestUninstallHookPostCommit_RemovesFileWhenStubOnly(t *testing.T) {
	repo := initRepo(t)
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !removed {
		t.Error("expected removed=true on fresh-install uninstall")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected hook file removed; stat returned %v", err)
	}
}

func TestUninstallHookPostCommit_Noop(t *testing.T) {
	repo := initRepo(t)
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if removed {
		t.Error("Uninstall on non-existent hook should report removed=false")
	}
	if path == "" {
		t.Error("Uninstall should still return resolved hook path")
	}
}

func TestInstallHook_PostMergeAndChurn(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-merge", InstallOpts{RegenChurn: true, ChurnBranch: "origin/main"})
	if err != nil {
		t.Fatalf("InstallHook post-merge: %v", err)
	}
	if filepath.Base(path) != "post-merge" {
		t.Errorf("expected post-merge hook file, got %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"# gortex-managed:post-merge:begin",
		"# gortex-managed:post-merge:end",
		"gortex enrich churn",
		`--branch="origin/main"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
	// Post-commit and post-merge should be independently managed.
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenChurn: true}); err != nil {
		t.Fatalf("InstallHook post-commit: %v", err)
	}
	if _, removed, err := UninstallHook(repo, "post-merge"); err != nil || !removed {
		t.Fatalf("UninstallHook post-merge removed=%v err=%v", removed, err)
	}
	// Post-commit hook should still exist after we uninstalled post-merge.
	postCommitPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	if _, err := os.Stat(postCommitPath); err != nil {
		t.Errorf("post-commit hook should survive post-merge uninstall: %v", err)
	}
}

func TestInstallHook_RegenReleases(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-merge", InstallOpts{
		RegenReleases:  true,
		ReleasesBranch: "origin/main",
	})
	if err != nil {
		t.Fatalf("InstallHook post-merge: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"gortex enrich releases",
		`--branch="origin/main"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
}

func TestInstallHook_RejectsUnsupportedHook(t *testing.T) {
	repo := initRepo(t)
	if _, err := InstallHook(repo, "pre-push", InstallOpts{RegenMermaid: true}); err == nil {
		t.Fatal("expected error for unsupported hook pre-push")
	}
}

func TestInstallHook_BoundedCallsWrapped(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-merge", InstallOpts{
		RegenMermaid:       true,
		RegenWiki:          true,
		RegenDocs:          true,
		RegenChurn:         true,
		ChurnBranch:        "origin/main",
		RegenReleases:      true,
		ReleasesBranch:     "origin/main",
		HookTimeoutSeconds: 7,
	})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"# Bound each gortex invocation so a busy daemon cannot hang git.",
		"gortex_hook_run() {",
		"timeout --version",
		"timeout -k 2",
		"command -v perl",
		"kill 9,$p",
		`gortex_hook_run 7 gortex enrich churn --branch="origin/main" >/dev/null 2>&1 || true`,
		`gortex_hook_run 7 gortex enrich releases --branch="origin/main" >/dev/null 2>&1 || true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
	if c := strings.Count(got, "gortex_hook_run 7 "); c != 5 {
		t.Errorf("expected 5 wrapped invocations (mermaid, wiki, docs, churn, releases), got %d. Body:\n%s", c, got)
	}
	if c := strings.Count(got, "gortex_hook_run() {"); c != 1 {
		t.Errorf("expected exactly one helper definition, got %d", c)
	}
	if i, j := strings.Index(got, "gortex_hook_run() {"), strings.Index(got, "gortex_hook_run 7 "); i == -1 || j == -1 || i > j {
		t.Errorf("helper must be defined before the first wrapped call. Body:\n%s", got)
	}
	if strings.Contains(got, "(gortex enrich churn)") {
		t.Errorf("bounded install must not emit legacy unwrapped lines. Body:\n%s", got)
	}
}

func TestInstallHook_ZeroTimeoutEmitsLegacyLines(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{RegenChurn: true})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	want := "(gortex enrich churn) >/dev/null 2>&1 || true"
	if !strings.Contains(got, want) {
		t.Errorf("zero timeout must emit legacy line %q. Body:\n%s", want, got)
	}
	if strings.Contains(got, "gortex_hook_run") {
		t.Errorf("zero timeout must not emit the watchdog helper. Body:\n%s", got)
	}
}

func TestInstallHook_NoActionsNoHelper(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{HookTimeoutSeconds: 30})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "# (no regeneration actions enabled)") {
		t.Errorf("no-actions install should note it explicitly. Body:\n%s", got)
	}
	if strings.Contains(got, "gortex_hook_run") {
		t.Errorf("no actions means no hang surface — helper must not ship. Body:\n%s", got)
	}
}

func TestInstallHook_PostCheckoutUnchangedByTimeout(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-checkout", InstallOpts{HookTimeoutSeconds: 30})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "touch .gortex/reindex.notify 2>/dev/null || true") {
		t.Errorf("post-checkout body must stay unchanged. Body:\n%s", got)
	}
	if strings.Contains(got, "gortex_hook_run") {
		t.Errorf("post-checkout has no gortex call — no helper expected. Body:\n%s", got)
	}
}

func TestHookPathFor_StaysInsideRepo(t *testing.T) {
	repo := initRepo(t)
	path, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	if !strings.HasPrefix(path, repo) {
		t.Errorf("hook path %q escapes the temp repo %q — a machine-global core.hooksPath would make every test write to the real global hooks dir", path, repo)
	}
}

func TestHookPathFor_HonoursCoreHooksPath(t *testing.T) {
	repo := initRepo(t)
	customHooks := filepath.Join(repo, "custom-hooks")
	if err := os.MkdirAll(customHooks, 0o755); err != nil {
		t.Fatalf("mkdir custom: %v", err)
	}
	cmd := exec.Command("git", "config", "core.hooksPath", customHooks)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config core.hooksPath: %v: %s", err, out)
	}
	path, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	if filepath.Dir(path) != customHooks {
		t.Errorf("HookPathFor should honour core.hooksPath, got %q under %q (want %q)",
			path, filepath.Dir(path), customHooks)
	}
}

func TestInstallHook_WatchdogKillsHangingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook E2E executes via sh; not supported on windows")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		if _, perr := exec.LookPath("perl"); perr != nil {
			t.Skip("neither timeout nor perl on PATH — watchdog cascade has nothing to drive")
		}
	}
	tmp := t.TempDir()
	// The shim must be a compiled Go program: the Go runtime swallows
	// SIGALRM, so a shell shim would pass under a broken (alarm-only)
	// watchdog — exactly the regression this test exists to catch.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH — cannot build the Go shim")
	}
	shimSrc := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(shimSrc, []byte("package main\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n\t\"time\"\n)\n\nfunc main() {\n\t_ = os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), \"ran\"), []byte(\"x\"), 0o644)\n\ttime.Sleep(60 * time.Second)\n}\n"), 0o644); err != nil {
		t.Fatalf("write shim source: %v", err)
	}
	shim := filepath.Join(tmp, "fake-gortex")
	build := exec.Command("go", "build", "-o", shim, shimSrc)
	build.Dir = tmp
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Go shim: %v: %s", err, out)
	}
	sentinel := filepath.Join(tmp, "ran")
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{
		RegenChurn:         true,
		Binary:             shim,
		HookTimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	start := time.Now()
	if _, err := exec.Command("sh", path).CombinedOutput(); err != nil {
		t.Fatalf("hook run: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("hook never invoked the shim binary (sentinel missing): %v", err)
	}
	// The killed call is swallowed by || true so the hook exits 0; the
	// assertion is purely that it did not take the shim's 60s.
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("hook took %v — watchdog did not kill the hanging shim", d)
	}
}

func TestInstallHook_WatchdogKillsHangingBinary_PerlLaneForced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hook E2E executes via sh; not supported on windows")
	}
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl not on PATH — forced perl lane has nothing to drive")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH — cannot build the Go shim")
	}
	// A `timeout` that exists but fails the capability probe must push the
	// cascade onto the perl lane — this is exactly the Git-for-Windows and
	// busybox shape, and the lane macOS runs for real.
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "timeout"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake timeout: %v", err)
	}
	tmp := t.TempDir()
	shimSrc := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(shimSrc, []byte("package main\n\nimport (\n\t\"os\"\n\t\"path/filepath\"\n\t\"time\"\n)\n\nfunc main() {\n\t_ = os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), \"ran\"), []byte(\"x\"), 0o644)\n\ttime.Sleep(60 * time.Second)\n}\n"), 0o644); err != nil {
		t.Fatalf("write shim source: %v", err)
	}
	shim := filepath.Join(tmp, "fake-gortex")
	build := exec.Command("go", "build", "-o", shim, shimSrc)
	build.Dir = tmp
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Go shim: %v: %s", err, out)
	}
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{
		RegenChurn:         true,
		Binary:             shim,
		HookTimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	start := time.Now()
	hookRun := exec.Command("sh", path)
	hookRun.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
	if _, err := hookRun.CombinedOutput(); err != nil {
		t.Fatalf("hook run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "ran")); err != nil {
		t.Fatalf("hook never invoked the shim binary (sentinel missing): %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("hook took %v — perl-lane watchdog did not kill the hanging Go shim", d)
	}
}

func TestInstallHook_BoundedBlockRoundTrip(t *testing.T) {
	repo := initRepo(t)
	for i := range 2 {
		if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenChurn: true, HookTimeoutSeconds: 5}); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if c := strings.Count(got, MarkerBegin); c != 1 {
		t.Errorf("expected one MarkerBegin after re-install, got %d", c)
	}
	if c := strings.Count(got, "gortex_hook_run() {"); c != 1 {
		t.Errorf("expected exactly one helper definition, got %d", c)
	}
	if _, removed, err := UninstallHook(repo, "post-commit"); err != nil || !removed {
		t.Fatalf("UninstallHook removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Errorf("stub-only bounded hook should be deleted on uninstall; stat returned %v", err)
	}
}

func TestInstallHook_LegacyToBoundedUpgrade(t *testing.T) {
	repo := initRepo(t)
	// Legacy install (zero timeout) — the shape every existing user has.
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenChurn: true}); err != nil {
		t.Fatalf("legacy install: %v", err)
	}
	// Bounded reinstall — the upgrade `gortex githook install` delivers.
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenChurn: true, HookTimeoutSeconds: 30}); err != nil {
		t.Fatalf("bounded reinstall: %v", err)
	}
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "gortex_hook_run 30 gortex enrich churn >/dev/null 2>&1 || true") {
		t.Errorf("upgrade must leave the bounded call. Body:\n%s", got)
	}
	if strings.Contains(got, "(gortex enrich churn) >/dev/null 2>&1 || true") {
		t.Errorf("upgrade must replace the legacy line. Body:\n%s", got)
	}
	if c := strings.Count(got, MarkerBegin); c != 1 {
		t.Errorf("expected one marker block after upgrade, got %d", c)
	}
}
