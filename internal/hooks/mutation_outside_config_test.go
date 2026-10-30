package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
)

type outsideConfigFixture struct{ primary, target, sibling string }

func newOutsideConfigFixture(t *testing.T) outsideConfigFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	f := outsideConfigFixture{filepath.Join(base, "primary"), filepath.Join(base, "target"), filepath.Join(base, "sibling")}
	require.NoError(t, os.Mkdir(f.primary, 0o755))
	writeFixtureFile(t, filepath.Join(f.primary, "a.go"), "package fixture\nfunc Target() {}\n")
	git := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		prefix := []string{"-c", "core.hooksPath=" + filepath.Join(base, "no-hooks"), "-c", "commit.gpgsign=false", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-C", f.primary}
		cmd := exec.CommandContext(ctx, "git", append(prefix, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		cmd.WaitDelay = time.Second
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	git("init", "--initial-branch=main")
	git("add", "a.go")
	git("commit", "-m", "Owned linked checkout fixture")
	git("worktree", "add", "--detach", f.target)
	git("worktree", "add", "--detach", f.sibling)
	// Load a real primary-only registry; neither linked checkout is added to
	// config to make the scope predicate pass.
	configPath := writeGlobalConfig(t, fmt.Sprintf("repos:\n  - path: %q\n", f.primary))
	repos, ok := loadTrackedReposFromPath(configPath)
	require.True(t, ok)
	require.Len(t, repos, 1)
	require.Equal(t, f.primary, repos[0].Path)
	previous := hookTrackedReposFn
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus { return repos }
	t.Cleanup(func() { hookTrackedReposFn = previous })
	require.Equal(t, f.target, linkedWorktreeRoot(filepath.Join(f.target, "a.go")))
	require.False(t, hookCallTargetsTrackedRepo("Read", map[string]any{"file_path": "a.go"}, f.target))
	return f
}

func outsideConfigDoors() []HookInput {
	return []HookInput{
		{ToolName: "Read", ToolInput: map[string]any{"file_path": "a.go"}},
		{ToolName: "Edit", ToolInput: map[string]any{"file_path": "a.go", "old_string": "Target", "new_string": "Changed"}},
		{ToolName: "Write", ToolInput: map[string]any{"file_path": "a.go", "content": "package changed\n"}},
		{ToolName: "Bash", ToolInput: map[string]any{"command": "cat a.go"}},
		{ToolName: "Bash", ToolInput: map[string]any{"command": "head -n 8 a.go"}},
		{ToolName: "Bash", ToolInput: map[string]any{"command": "tail -n 8 a.go"}},
	}
}

func TestOutsideConfigReadOnlyRecoveryFileDoorsUseOneProbe(t *testing.T) {
	f := newOutsideConfigFixture(t)
	for _, input := range outsideConfigDoors() {
		for _, block := range []bool{false, true} {
			for _, mode := range []Mode{ModeDeny, ModeEnrich, ModeConsultUnlock, ModeAdaptiveNudge} {
				name := fmt.Sprintf("%s/%v/block%v/%s", input.ToolName, input.ToolInput, block, mode)
				t.Run(name, func(t *testing.T) {
					withEditBlocking(t, block)
					input.CWD, input.SessionID = f.target, "outside-config"
					calls := 0
					// Count the underlying query seam, beneath the invocation's
					// memoized/session probe. Production maps it to one file_coverage.
					stubFileIndexScopeBy(t, func(cwd, path string) fileIndexStatus {
						calls++
						require.Equal(t, f.target, cwd)
						require.Equal(t, "a.go", path, "preserve the original probe/cache key")
						return fileIndexStatusFromCoverage(daemon.FileCoverageResult{Answered: true, Covered: true, Tracked: true, Symbols: 2, MutationState: "read_only", View: &daemon.ProbeView{CheckoutID: "target", Exact: true}})
					})
					result := applyMode(input, false, mode, enrich(input, 0))
					require.False(t, result.deny)
					require.True(t, result.readOnlyAllowance)
					require.Contains(t, result.context, "native reads and edits are allowed")
					require.Contains(t, result.context, "fresh view")
					require.Contains(t, result.context, "reconnect the MCP session")
					require.Equal(t, input.ToolName == "Edit" || input.ToolName == "Write", strings.Contains(result.context, "git diff and the tests"))
					require.Equal(t, 1, calls)
				})
			}
		}
	}
}

func TestOutsideConfigRecoveryKeepsReadyAndUnknownDoorsSilent(t *testing.T) {
	f := newOutsideConfigFixture(t)
	for _, input := range outsideConfigDoors() {
		for name, status := range map[string]fileIndexStatus{
			"ready":           {Indexed: true, Count: 2, Tracked: true, ProbeOK: true, MutationKnown: true},
			"legacy_no_mark":  {Indexed: true, Count: 2, Tracked: true, ProbeOK: true},
			"untracked":       {ProbeOK: true},
			"loading":         {Loading: true, Tracked: true, ProbeOK: true},
			"unknown":         {Tracked: true},
			"unreached":       {Unreached: true},
			"failed_readonly": {ReadOnly: true, Unreached: true, ProbeOK: true},
			"unproven_mark":   {ReadOnly: true},
		} {
			t.Run(fmt.Sprintf("%s/%v/%s", input.ToolName, input.ToolInput, name), func(t *testing.T) {
				input.CWD, input.SessionID = f.target, "no-legacy-mark"
				calls := 0
				stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus { calls++; return status })
				result := enrich(input, 0)
				require.Equal(t, enrichResult{}, result)
				require.Equal(t, 1, calls)
			})
		}
	}
}

func TestOutsideConfigRecoveryDoesNotExpandOtherScopesOrShellShapes(t *testing.T) {
	f := newOutsideConfigFixture(t)
	calls := 0
	stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus { calls++; return fileIndexStatus{ReadOnly: true, ProbeOK: true} })
	for _, input := range []HookInput{
		{ToolName: "Read", CWD: t.TempDir(), ToolInput: map[string]any{"file_path": "a.go"}},
		{ToolName: "Read", CWD: f.target, ToolInput: map[string]any{"file_path": "README.md"}},
		{ToolName: "Grep", CWD: f.target, ToolInput: map[string]any{"pattern": "Target"}},
		{ToolName: "Glob", CWD: f.target, ToolInput: map[string]any{"pattern": "*.go"}},
		{ToolName: "Bash", CWD: f.target, ToolInput: map[string]any{"command": "sed -n '1,4p' a.go"}},
		{ToolName: "Bash", CWD: f.target, ToolInput: map[string]any{"command": "grep Target a.go"}},
		{ToolName: "Bash", CWD: f.target, ToolInput: map[string]any{"command": "printf x > a.go"}},
		{ToolName: "Bash", CWD: f.target, ToolInput: map[string]any{"command": "cat $UNKNOWN"}},
	} {
		require.Equal(t, enrichResult{}, enrich(input, 0), "%+v", input)
	}
	require.Zero(t, calls, "ordinary outside paths and non-file doors must not acquire probes")
	// Missing cwd is unproven to the existing global scope gate, not proof of
	// being outside config. The recovery-only helper itself must not guess it.
	require.Equal(t, enrichResult{}, outsideConfiguredReadOnlyRecovery(HookInput{ToolName: "Read", ToolInput: map[string]any{"file_path": "a.go"}}, queryFileIndexScope))
	require.Zero(t, calls)
	// A forged pointer is only a candidate, never proof of graph membership.
	fake := t.TempDir()
	writeFixtureFile(t, filepath.Join(fake, ".git"), "gitdir: /missing/worktrees/forged\n")
	stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus { calls++; return fileIndexStatus{ProbeOK: true} })
	require.Equal(t, enrichResult{}, enrich(HookInput{ToolName: "Read", CWD: fake, ToolInput: map[string]any{"file_path": "a.go"}}, 0))
	require.Equal(t, 1, calls)
}

func TestOutsideConfigRecoveryLeavesConfiguredReadyRedirectIntact(t *testing.T) {
	f := newOutsideConfigFixture(t)
	calls := 0
	stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus {
		calls++
		return fileIndexStatus{Indexed: true, Count: 2, Tracked: true, ProbeOK: true, MutationKnown: true}
	})
	result := enrich(HookInput{ToolName: "Read", CWD: f.primary, ToolInput: map[string]any{"file_path": "a.go"}}, 0)
	require.True(t, result.deny)
	require.False(t, result.readOnlyAllowance)
	require.Equal(t, 1, calls)
}

func TestOutsideConfigRecoveryDoesNotBorrowTargetRefusalForReadySibling(t *testing.T) {
	f := newOutsideConfigFixture(t)
	calls := 0
	stubFileIndexScopeBy(t, func(cwd, path string) fileIndexStatus {
		calls++
		abs, ok := hookAbsScopePath(path, cwd)
		require.True(t, ok)
		return fileIndexStatus{Indexed: true, Count: 2, Tracked: true, ProbeOK: true, MutationKnown: true, ReadOnly: abs == filepath.Join(f.target, "a.go")}
	})
	for _, root := range []string{f.target, f.sibling} {
		before := calls
		result := enrich(HookInput{ToolName: "Read", CWD: root, ToolInput: map[string]any{"file_path": "a.go"}}, 0)
		require.False(t, result.deny)
		require.Equal(t, root == f.target, result.readOnlyAllowance)
		require.Equal(t, before+1, calls)
	}
}

func TestOutsideConfigRecoveryLegacyProofStillBelongsToExactCheckoutSession(t *testing.T) {
	f := newOutsideConfigFixture(t)
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	require.True(t, observeMutationFallback(mutationPostFixture(f.target, "owner", true, legacyStaleRefusal), time.Now()))
	for _, tc := range []struct {
		name, cwd, session string
		status             fileIndexStatus
		allowed            bool
	}{
		{"owner", f.target, "owner", fileIndexStatus{Indexed: true, Tracked: true, ProbeOK: true}, true},
		{"sibling", f.sibling, "owner", fileIndexStatus{Indexed: true, Tracked: true, ProbeOK: true}, false},
		{"other_session", f.target, "other", fileIndexStatus{Indexed: true, Tracked: true, ProbeOK: true}, false},
		{"modern_ready", f.target, "owner", fileIndexStatus{Indexed: true, Tracked: true, ProbeOK: true, MutationKnown: true}, false},
		{"unreached", f.target, "owner", fileIndexStatus{Unreached: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus { calls++; return tc.status })
			result := enrich(HookInput{ToolName: "Read", CWD: tc.cwd, SessionID: tc.session, ToolInput: map[string]any{"file_path": "a.go"}}, 0)
			require.False(t, result.deny)
			require.Equal(t, tc.allowed, result.readOnlyAllowance)
			require.Equal(t, 1, calls)
		})
	}
	st := loadSessionState("owner")
	for root := range st.MutationRefusals {
		st.MutationRefusals[root] = time.Now().Add(-mutationRefusalFallbackTTL - time.Minute).UnixNano()
	}
	saveSessionState("owner", st)
	stubFileIndexScope(t, fileIndexStatus{Indexed: true, Tracked: true, ProbeOK: true})
	require.Equal(t, enrichResult{}, enrich(HookInput{ToolName: "Read", CWD: f.target, SessionID: "owner", ToolInput: map[string]any{"file_path": "a.go"}}, 0))
}
