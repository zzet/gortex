package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

// snapshotTree records every entry below root so a dry run can be proven to
// have written nothing at all — not merely to have left the named files alone.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		switch {
		case entry.IsDir():
			tree[rel] = "dir"
		case entry.Type()&os.ModeSymlink != 0:
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				return linkErr
			}
			tree[rel] = "link:" + target
		default:
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			tree[rel] = "file:" + testSHA256(string(content))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// requireNoTransactionState asserts the dry run never created the journal
// directory. Tolerating an existing empty root would also pass a dry run that
// opened a transaction and tidied up afterwards, which is not the claim.
func requireNoTransactionState(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created transaction state at %s: %v", dir, err)
	}
}

// isolateGitConfig keeps a classification test off the developer's global and
// system git configuration: an inherited ignore file or quoting setting would
// otherwise decide what the test measures. The default core.excludesFile is
// not named by any config file, so emptying the config is not enough — git
// reads $XDG_CONFIG_HOME/git/ignore, falling back to $HOME/.config/git/ignore,
// and both are pointed at empty directories.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

// initGitRepo creates a repository at root and commits the named paths.
func initGitRepo(t *testing.T, root string, commit ...string) {
	t.Helper()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init")
	if len(commit) == 0 {
		return
	}
	// `git add` takes pathspecs, so a name starting with ':' has to be spelled
	// literally — the same reason the classifier prefixes its own ls-files
	// arguments.
	args := []string{"add", "--"}
	for _, path := range commit {
		args = append(args, ":(literal)"+path)
	}
	runGit(args...)
	runGit("-c", "user.name=Gortex", "-c", "user.email=gortex@example.com", "commit", "-m", "initial")
}

// dryRunPlan drives the dry-run branch and returns the decoded response plus
// its plan entries.
func dryRunPlan(t *testing.T, s *Server, edits []any) (map[string]any, []map[string]any) {
	t.Helper()
	return decodeDryRunPlan(t, callBatchEdit(t, s, map[string]any{"edits": edits, "dry_run": true}))
}

// dryRunRequest is the call dryRunPlan makes, for a test that has to drive the
// handler itself — off the test goroutine, or with a context of its own.
func dryRunRequest(edits []any) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Name = "batch_edit"
	req.Params.Arguments = map[string]any{"edits": edits, "dry_run": true}
	return req
}

// decodeDryRunPlan renders one dry-run response as the decoded object plus its
// plan entries, so every dry-run assertion reads the same shape however the
// call was made.
func decodeDryRunPlan(t *testing.T, res *mcplib.CallToolResult) (map[string]any, []map[string]any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("dry run failed: %s", readText(t, res))
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp); err != nil {
		t.Fatal(err)
	}
	raw, ok := resp["plan"].([]any)
	if !ok {
		t.Fatalf("response has no plan: %v", resp)
	}
	plan := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("plan entry is not an object: %v", entry)
		}
		plan = append(plan, item)
	}
	return resp, plan
}

func planStatus(t *testing.T, entry map[string]any) string {
	t.Helper()
	status, _ := entry["status"].(string)
	return status
}

func planState(t *testing.T, entry map[string]any, key string) map[string]any {
	t.Helper()
	state, ok := entry[key].(map[string]any)
	if !ok {
		t.Fatalf("plan entry has no %s state: %v", key, entry)
	}
	return state
}

func TestBatchEditDryRunReportsLifecycleConflicts(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, repoRoot string) []any
		want  string
		// wantConflicts defaults to one; an aliasing refusal names two paths
		// and both items carry it.
		wantConflicts int
	}{
		{
			name: "missing source",
			setup: func(t *testing.T, repoRoot string) []any {
				return []any{map[string]any{
					"op": "delete_file", "path": filepath.Join(repoRoot, "absent.txt"),
				}}
			},
			want: "conflict: source file does not exist",
		},
		{
			name: "destination exists",
			setup: func(t *testing.T, repoRoot string) []any {
				source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
				destination := writeAtomicBatchFixture(t, repoRoot, "destination.txt", "destination\n")
				return []any{map[string]any{
					"op": "move_file", "source": source, "destination": destination,
				}}
			},
			want: "conflict: destination already exists",
		},
		{
			name: "symlinked destination parent",
			setup: func(t *testing.T, repoRoot string) []any {
				if os.PathSeparator == '\\' {
					t.Skip("symlink creation is not reliably available on Windows CI")
				}
				source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
				realParent := filepath.Join(repoRoot, "real-parent")
				if err := os.Mkdir(realParent, 0o755); err != nil {
					t.Fatal(err)
				}
				linkedParent := filepath.Join(repoRoot, "linked-parent")
				if err := os.Symlink(realParent, linkedParent); err != nil {
					t.Fatal(err)
				}
				return []any{map[string]any{
					"op": "move_file", "source": source,
					"destination": filepath.Join(linkedParent, "destination.txt"),
				}}
			},
			// The guard's own wording, so the source-symlink refusal cannot
			// satisfy this case by accident.
			want: "conflict: move destination contains symlink component",
		},
		{
			name: "source is a symlink",
			setup: func(t *testing.T, repoRoot string) []any {
				if os.PathSeparator == '\\' {
					t.Skip("symlink creation is not reliably available on Windows CI")
				}
				target := writeAtomicBatchFixture(t, repoRoot, "target.txt", "target\n")
				link := filepath.Join(repoRoot, "link.txt")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				return []any{map[string]any{"op": "delete_file", "path": link}}
			},
			want: "conflict: source path is a symlink",
		},
		{
			name: "aliased paths",
			setup: func(t *testing.T, repoRoot string) []any {
				source := writeAtomicBatchFixture(t, repoRoot, "a.txt", "source\n")
				alias := filepath.Join(repoRoot, "b.txt")
				if err := os.Link(source, alias); err != nil {
					t.Skipf("hard links unavailable: %v", err)
				}
				return []any{
					map[string]any{
						"op": "edit_file", "path": source,
						"old_string": "source", "new_string": "edited",
					},
					map[string]any{"op": "delete_file", "path": alias},
				}
			},
			want:          "name the same file",
			wantConflicts: 2,
		},
		{
			name: "digest mismatch",
			setup: func(t *testing.T, repoRoot string) []any {
				source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
				return []any{map[string]any{
					"op": "delete_file", "path": source,
					"expected_sha256": strings.Repeat("0", 64),
				}}
			},
			want: "conflict: expected_sha256 does not match complete source bytes",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			transactions := filepath.Join(t.TempDir(), "transactions")
			t.Setenv(batchTransactionDirEnv, transactions)
			s := newReadGuardServer(t, repoRoot)
			edits := testCase.setup(t, repoRoot)
			before := snapshotTree(t, repoRoot)

			resp, plan := dryRunPlan(t, s, edits)
			reported := make([]string, 0, len(plan))
			for _, entry := range plan {
				if status := planStatus(t, entry); strings.HasPrefix(status, "conflict: ") {
					reported = append(reported, status)
				}
			}
			wantConflicts := testCase.wantConflicts
			if wantConflicts == 0 {
				wantConflicts = 1
			}
			if len(reported) != wantConflicts {
				t.Fatalf("conflict statuses = %v, want %d; plan = %v", reported, wantConflicts, plan)
			}
			for _, status := range reported {
				if !strings.Contains(status, testCase.want) {
					t.Fatalf("status = %q, want a conflict containing %q", status, testCase.want)
				}
			}
			if conflicts, _ := resp["conflicts"].(float64); int(conflicts) != len(reported) {
				t.Fatalf("conflicts = %v, want %d", resp["conflicts"], len(reported))
			}
			if after := snapshotTree(t, repoRoot); !reflect.DeepEqual(before, after) {
				t.Fatalf("dry run changed disk: before=%v after=%v", before, after)
			}
			requireNoTransactionState(t, transactions)
		})
	}
}

func TestBatchEditDryRunPlansHealthyMove(t *testing.T) {
	repoRoot := t.TempDir()
	transactions := filepath.Join(t.TempDir(), "transactions")
	t.Setenv(batchTransactionDirEnv, transactions)
	s := newReadGuardServer(t, repoRoot)
	source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "move me\n")
	destination := filepath.Join(repoRoot, "nested", "destination.txt")
	before := snapshotTree(t, repoRoot)

	resp, plan := dryRunPlan(t, s, []any{map[string]any{
		"op": "move_file", "source": source, "destination": destination,
		"expected_sha256": testSHA256("move me\n"),
	}})
	if len(plan) != 1 {
		t.Fatalf("plan = %v", plan)
	}
	entry := plan[0]
	if status := planStatus(t, entry); status != "planned" {
		t.Fatalf("status = %q, want planned", status)
	}
	if conflicts, _ := resp["conflicts"].(float64); conflicts != 0 {
		t.Fatalf("conflicts = %v, want 0", resp["conflicts"])
	}
	if got := entry["resolved_path"]; got != source {
		t.Fatalf("resolved_path = %v, want %s", got, source)
	}
	if got := entry["resolved_destination"]; got != destination {
		t.Fatalf("resolved_destination = %v, want %s", got, destination)
	}
	// The state objects ride on their own keys: `destination` stays the
	// relative destination path every plan entry already carries.
	if got := entry["destination"]; got != "nested/destination.txt" {
		t.Fatalf("destination = %v, want the relative destination path", got)
	}
	sourceState := planState(t, entry, "source_state")
	if sourceState["exists"] != true || sourceState["kind"] != "regular" {
		t.Fatalf("source state = %v", sourceState)
	}
	if got := sourceState["sha256"]; got != testSHA256("move me\n") {
		t.Fatalf("source sha256 = %v, want %s", got, testSHA256("move me\n"))
	}
	destinationState := planState(t, entry, "destination_state")
	if destinationState["exists"] != false || destinationState["kind"] != "absent" {
		t.Fatalf("destination state = %v", destinationState)
	}
	if after := snapshotTree(t, repoRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run changed disk: before=%v after=%v", before, after)
	}
	requireNoTransactionState(t, transactions)
}

func TestBatchEditDryRunClassifiesGitState(t *testing.T) {
	isolateGitConfig(t)
	repoRoot := t.TempDir()
	writeAtomicBatchFixture(t, repoRoot, ".gitignore", "proposal/\n*.log\n")
	tracked := writeAtomicBatchFixture(t, repoRoot, "tracked.txt", "tracked\n")
	initGitRepo(t, repoRoot, ".gitignore", "tracked.txt")
	untracked := writeAtomicBatchFixture(t, repoRoot, "untracked.txt", "untracked\n")
	ignoredFile := writeAtomicBatchFixture(t, repoRoot, "run.log", "log\n")
	proposal := filepath.Join(repoRoot, "proposal")
	if err := os.Mkdir(proposal, 0o755); err != nil {
		t.Fatal(err)
	}
	draft := writeAtomicBatchFixture(t, proposal, "draft.txt", "draft\n")
	absentDestination := filepath.Join(proposal, "moved.txt")

	transactions := filepath.Join(t.TempDir(), "transactions")
	t.Setenv(batchTransactionDirEnv, transactions)
	s := newReadGuardServer(t, repoRoot)
	before := snapshotTree(t, repoRoot)

	resp, plan := dryRunPlan(t, s, []any{
		map[string]any{"op": "delete_file", "path": tracked},
		map[string]any{"op": "delete_file", "path": untracked},
		map[string]any{"op": "delete_file", "path": ignoredFile},
		map[string]any{"op": "move_file", "source": draft, "destination": absentDestination},
	})
	if conflicts, _ := resp["conflicts"].(float64); conflicts != 0 {
		t.Fatalf("conflicts = %v, want 0", resp["conflicts"])
	}
	states := make(map[string]map[string]any, len(plan))
	for _, entry := range plan {
		resolved, _ := entry["resolved_path"].(string)
		states[resolved] = planState(t, entry, "source_state")
		if destination, ok := entry["resolved_destination"].(string); ok {
			states[destination] = planState(t, entry, "destination_state")
		}
	}
	for path, want := range map[string]struct {
		git     string
		ignored bool
	}{
		tracked:           {git: "tracked"},
		untracked:         {git: "untracked"},
		ignoredFile:       {git: "ignored", ignored: true},
		draft:             {git: "ignored", ignored: true},
		absentDestination: {git: "absent", ignored: true},
	} {
		state, ok := states[path]
		if !ok {
			t.Fatalf("no state reported for %s: %v", path, states)
		}
		if state["git"] != want.git || state["ignored"] != want.ignored {
			t.Errorf("%s state = %v, want git=%q ignored=%v", path, state, want.git, want.ignored)
		}
	}
	if after := snapshotTree(t, repoRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run changed disk")
	}
	requireNoTransactionState(t, transactions)
}

func TestBatchEditDryRunReportsUnknownGitStateOutsideRepository(t *testing.T) {
	repoRoot := t.TempDir()
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)
	source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")

	_, plan := dryRunPlan(t, s, []any{map[string]any{"op": "delete_file", "path": source}})
	if len(plan) != 1 {
		t.Fatalf("plan = %v", plan)
	}
	if status := planStatus(t, plan[0]); status != "planned" {
		t.Fatalf("status = %q, want planned", status)
	}
	state := planState(t, plan[0], "source_state")
	if state["git"] != "unknown" || state["ignored"] != false {
		t.Fatalf("source state = %v, want git=unknown ignored=false", state)
	}
}

// TestBatchEditDryRunReportsUnknownGitStateWithoutGitBinary pins the claim that
// the classification is evidence and never a gate. With no git on PATH there is
// nothing to shell out to, and the dry run still has to answer with its plan
// rather than fail, hang, or panic on the missing binary. The fixture is a real
// repository with the path committed: git would answer "tracked", so the
// missing binary is the only thing this test can be measuring.
func TestBatchEditDryRunReportsUnknownGitStateWithoutGitBinary(t *testing.T) {
	isolateGitConfig(t)
	repoRoot := t.TempDir()
	source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
	initGitRepo(t, repoRoot, "source.txt")
	t.Setenv("PATH", t.TempDir())
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)

	_, plan := dryRunPlan(t, s, []any{map[string]any{"op": "delete_file", "path": source}})
	if len(plan) != 1 {
		t.Fatalf("plan = %v", plan)
	}
	if status := planStatus(t, plan[0]); status != "planned" {
		t.Fatalf("status = %q, want planned", status)
	}
	state := planState(t, plan[0], "source_state")
	if state["git"] != "unknown" || state["ignored"] != false {
		t.Fatalf("source state = %v, want git=unknown ignored=false", state)
	}
}

// TestBatchEditDryRunBoundsSlowGitClassification pins the per-checkout git
// timeout. A wedged repository is the case the bound exists for: git never
// answers, and the dry run still owes its caller a plan with the honest
// "unknown" rather than the caller's whole call. The fake git sleeps far longer
// than the bound, so without it the handler would return only when the sleep
// does — the select below is what turns that into a fast failure instead of a
// hung suite.
func TestBatchEditDryRunBoundsSlowGitClassification(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("the stand-in git is a POSIX shell script")
	}
	isolateGitConfig(t)
	repoRoot := t.TempDir()
	source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
	initGitRepo(t, repoRoot, "source.txt")

	stubDir := t.TempDir()
	stub := []byte("#!/bin/sh\nexec /bin/sleep 20\n")
	if err := os.WriteFile(filepath.Join(stubDir, "git"), stub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)

	// The handler is driven directly for the same reason as above: this
	// goroutine has to be the one that gives up, and a t.Fatal from the other
	// one would not fail the test.
	done := make(chan *mcplib.CallToolResult, 1)
	go func() {
		res, handleErr := s.handleAtomicBatchEdit(context.Background(), dryRunRequest([]any{
			map[string]any{"op": "delete_file", "path": source},
		}))
		if handleErr != nil {
			done <- nil
			return
		}
		done <- res
	}()

	select {
	case res := <-done:
		if res == nil {
			t.Fatal("dry run returned a handler error")
		}
		_, plan := decodeDryRunPlan(t, res)
		if len(plan) != 1 || planStatus(t, plan[0]) != "planned" {
			t.Fatalf("plan = %v", plan)
		}
		state := planState(t, plan[0], "source_state")
		if state["git"] != "unknown" {
			t.Fatalf("source state = %v, want git=unknown", state)
		}
	case <-time.After(4 * batchLifecycleGitTimeout):
		// The deadline only has to sit far below the mutant's failure time —
		// the stand-in git sleeps for 20 s — not close to the expected 3 s, so
		// a slow CI runner does not turn a passing bound into a flake.
		t.Fatalf("dry run outlived the %s git classification bound", batchLifecycleGitTimeout)
	}
}

// TestBatchEditDryRunTakesNoMutationPathLock pins the advisory claim: the
// preflight reads disk without serialising against a writer, so an agent can
// ask "would this commit?" while another mutation holds the very paths it asks
// about. Holding those locks for the whole call and still getting an answer is
// the only way to prove no lock is taken.
func TestBatchEditDryRunTakesNoMutationPathLock(t *testing.T) {
	repoRoot := t.TempDir()
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)
	source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
	destination := filepath.Join(repoRoot, "destination.txt")

	release, err := acquireMutationPaths(context.Background(), []string{source, destination})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// The handler is driven directly: a t.Fatal from another goroutine would
	// not fail this test, so the goroutine only ferries the response back.
	done := make(chan *mcplib.CallToolResult, 1)
	go func() {
		res, handleErr := s.handleAtomicBatchEdit(context.Background(), dryRunRequest([]any{map[string]any{
			"op": "move_file", "source": source, "destination": destination,
		}}))
		if handleErr != nil {
			done <- nil
			return
		}
		done <- res
	}()

	select {
	case res := <-done:
		if res == nil {
			t.Fatal("dry run returned a handler error")
		}
		_, plan := decodeDryRunPlan(t, res)
		if len(plan) != 1 || planStatus(t, plan[0]) != "planned" {
			t.Fatalf("plan = %v", plan)
		}
	case <-time.After(30 * time.Second):
		// A dry run that does take the lock blocks indefinitely, so the
		// deadline can be generous without weakening the check; it only needs
		// to stay well clear of a slow runner's ordinary dry-run latency.
		t.Fatal("dry run blocked on a mutation path lock held by another caller")
	}
}

// TestBatchEditDryRunAnswersUnderACancelledRequest pins the same posture for a
// cancelled request: git classification is the only part that needs the
// context, so it degrades to "unknown" while the plan itself is still returned.
func TestBatchEditDryRunAnswersUnderACancelledRequest(t *testing.T) {
	isolateGitConfig(t)
	repoRoot := t.TempDir()
	tracked := writeAtomicBatchFixture(t, repoRoot, "tracked.txt", "tracked\n")
	initGitRepo(t, repoRoot, "tracked.txt")
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := s.handleAtomicBatchEdit(ctx, dryRunRequest([]any{
		map[string]any{"op": "delete_file", "path": tracked},
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, plan := decodeDryRunPlan(t, res)
	if len(plan) != 1 {
		t.Fatalf("plan = %v", plan)
	}
	if status := planStatus(t, plan[0]); status != "planned" {
		t.Fatalf("status = %q, want planned", status)
	}
	state := planState(t, plan[0], "source_state")
	if state["git"] != "unknown" || state["ignored"] != false {
		t.Fatalf("source state = %v, want git=unknown ignored=false", state)
	}
}

// planStates indexes a plan by resolved source path so a multi-item assertion
// can name the fixture instead of the sorted plan position.
func planStates(t *testing.T, plan []map[string]any) map[string]map[string]any {
	t.Helper()
	states := make(map[string]map[string]any, len(plan))
	for _, entry := range plan {
		resolved, _ := entry["resolved_path"].(string)
		states[resolved] = planState(t, entry, "source_state")
	}
	return states
}

// TestBatchEditDryRunClassifiesGitPathSpellings pins the spellings git itself
// does not round-trip: a leading ':' is pathspec magic on the ls-files side,
// check-ignore C-quotes a name containing a quote or a backslash whatever
// core.quotePath says, and a name that is already spelled like a quoted record
// must survive the NUL-separated stream, where git quotes nothing.
func TestBatchEditDryRunClassifiesGitPathSpellings(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip(`'"' and ':' are not valid in Windows file names`)
	}
	isolateGitConfig(t)
	repoRoot := t.TempDir()
	writeAtomicBatchFixture(t, repoRoot, ".gitignore", "*.log\n")
	colon := writeAtomicBatchFixture(t, repoRoot, ":colon.txt", "colon\n")
	quoted := writeAtomicBatchFixture(t, repoRoot, `q"uote.log`, "quoted\n")
	selfQuoted := writeAtomicBatchFixture(t, repoRoot, `"q"`, "self-quoted\n")
	// A space is nothing to git and everything to a shell, and a non-ASCII name
	// is what core.quotePath=false is set for: both spellings have to come back
	// as they were asked about, tracked and ignored alike.
	spaced := writeAtomicBatchFixture(t, repoRoot, "spaced name.txt", "spaced\n")
	spacedIgnored := writeAtomicBatchFixture(t, repoRoot, "spaced name.log", "spaced log\n")
	unicode := writeAtomicBatchFixture(t, repoRoot, "naïve.txt", "naive\n")
	unicodeIgnored := writeAtomicBatchFixture(t, repoRoot, "naïve.log", "naive log\n")
	initGitRepo(t, repoRoot, ".gitignore", ":colon.txt", `"q"`, "spaced name.txt", "naïve.txt")

	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)
	_, plan := dryRunPlan(t, s, []any{
		map[string]any{"op": "delete_file", "path": colon},
		map[string]any{"op": "delete_file", "path": quoted},
		map[string]any{"op": "delete_file", "path": selfQuoted},
		map[string]any{"op": "delete_file", "path": spaced},
		map[string]any{"op": "delete_file", "path": spacedIgnored},
		map[string]any{"op": "delete_file", "path": unicode},
		map[string]any{"op": "delete_file", "path": unicodeIgnored},
	})
	states := planStates(t, plan)
	for path, want := range map[string]struct {
		git     string
		ignored bool
	}{
		colon:          {git: "tracked"},
		quoted:         {git: "ignored", ignored: true},
		selfQuoted:     {git: "tracked"},
		spaced:         {git: "tracked"},
		spacedIgnored:  {git: "ignored", ignored: true},
		unicode:        {git: "tracked"},
		unicodeIgnored: {git: "ignored", ignored: true},
	} {
		state, ok := states[path]
		if !ok {
			t.Fatalf("no state reported for %s: %v", path, states)
		}
		if state["git"] != want.git || state["ignored"] != want.ignored {
			t.Errorf("%s state = %v, want git=%q ignored=%v", path, state, want.git, want.ignored)
		}
	}
}

// TestBatchEditDryRunClassifiesRepositoryWithNothingIgnored covers the
// documented "no path matched" exit status of git check-ignore: a repository
// with no ignore rules at all must still classify its paths, not degrade the
// whole group to unknown.
func TestBatchEditDryRunClassifiesRepositoryWithNothingIgnored(t *testing.T) {
	isolateGitConfig(t)
	repoRoot := t.TempDir()
	tracked := writeAtomicBatchFixture(t, repoRoot, "tracked.txt", "tracked\n")
	initGitRepo(t, repoRoot, "tracked.txt")
	untracked := writeAtomicBatchFixture(t, repoRoot, "untracked.txt", "untracked\n")

	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)
	_, plan := dryRunPlan(t, s, []any{
		map[string]any{"op": "delete_file", "path": tracked},
		map[string]any{"op": "delete_file", "path": untracked},
	})
	states := planStates(t, plan)
	for path, want := range map[string]string{tracked: "tracked", untracked: "untracked"} {
		state, ok := states[path]
		if !ok {
			t.Fatalf("no state reported for %s: %v", path, states)
		}
		if state["git"] != want || state["ignored"] != false {
			t.Errorf("%s state = %v, want git=%q ignored=false", path, state, want)
		}
	}
}

// TestBatchEditDryRunKeepsClassificationForPathsGitAnswers pins the blast
// radius of a pathname git will not answer for. `check-ignore` refuses the
// whole invocation with "pathspec … is beyond a symbolic link" — exit 128 —
// when one pathname crosses a symlinked directory, and a single such path in
// the batch must not cost every other path in the same checkout its answer.
func TestBatchEditDryRunKeepsClassificationForPathsGitAnswers(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	isolateGitConfig(t)
	repoRoot := t.TempDir()
	tracked := writeAtomicBatchFixture(t, repoRoot, "tracked.txt", "tracked\n")
	initGitRepo(t, repoRoot, "tracked.txt")
	real := filepath.Join(repoRoot, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAtomicBatchFixture(t, real, "x.txt", "beyond\n")
	if err := os.Symlink(real, filepath.Join(repoRoot, "linked")); err != nil {
		t.Fatal(err)
	}
	beyond := filepath.Join(repoRoot, "linked", "x.txt")

	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)
	_, plan := dryRunPlan(t, s, []any{
		map[string]any{"op": "delete_file", "path": tracked},
		map[string]any{"op": "delete_file", "path": beyond},
	})
	states := planStates(t, plan)
	trackedState, ok := states[tracked]
	if !ok {
		t.Fatalf("no state reported for %s: %v", tracked, states)
	}
	if trackedState["git"] != "tracked" || trackedState["ignored"] != false {
		t.Errorf("%s state = %v, want git=\"tracked\" ignored=false", tracked, trackedState)
	}
	// The refused path keeps the only honest answer there is for it.
	beyondState, ok := states[beyond]
	if !ok {
		t.Fatalf("no state reported for %s: %v", beyond, states)
	}
	if beyondState["git"] != "unknown" {
		t.Errorf("%s state = %v, want git=\"unknown\"", beyond, beyondState)
	}
}

// TestBatchEditDryRunResolvesSymbolPaths proves a dry run addresses the same
// files the commit does. A symbol edit only has a path once the plan resolves
// one, and until it does neither the lifecycle-overlap rule nor the aliasing
// rule has anything to compare it against.
func TestBatchEditDryRunResolvesSymbolPaths(t *testing.T) {
	symbolEdit := map[string]any{
		"id": "main.go::helper", "old_source": "func helper() {}", "new_source": "func helper() { _ = 1 }",
	}

	t.Run("overlapping lifecycle op on the symbol's own file", func(t *testing.T) {
		transactions := filepath.Join(t.TempDir(), "transactions")
		t.Setenv(batchTransactionDirEnv, transactions)
		srv, dir := setupTestServer(t)
		before := snapshotTree(t, dir)

		resp, plan := dryRunPlan(t, srv, []any{symbolEdit,
			map[string]any{"op": "delete_file", "path": filepath.Join(dir, "main.go")}})
		refused := make([]string, 0, len(plan))
		for _, entry := range plan {
			if status := planStatus(t, entry); status != "planned" {
				refused = append(refused, status)
			}
		}
		if len(refused) != 1 || !strings.Contains(refused[0], "overlaps batch item") {
			t.Fatalf("statuses = %v, want one overlap refusal; plan = %v", refused, plan)
		}
		if conflicts, _ := resp["conflicts"].(float64); int(conflicts) != 1 {
			t.Fatalf("conflicts = %v, want 1", resp["conflicts"])
		}
		if after := snapshotTree(t, dir); !reflect.DeepEqual(before, after) {
			t.Fatalf("dry run changed disk: before=%v after=%v", before, after)
		}
		requireNoTransactionState(t, transactions)
	})

	t.Run("lifecycle op through a hard link to the symbol's file", func(t *testing.T) {
		transactions := filepath.Join(t.TempDir(), "transactions")
		t.Setenv(batchTransactionDirEnv, transactions)
		srv, dir := setupTestServer(t)
		alias := filepath.Join(dir, "alias.go")
		if err := os.Link(filepath.Join(dir, "main.go"), alias); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		before := snapshotTree(t, dir)

		resp, plan := dryRunPlan(t, srv, []any{symbolEdit,
			map[string]any{"op": "delete_file", "path": alias}})
		for _, entry := range plan {
			if status := planStatus(t, entry); !strings.Contains(status, "name the same file") {
				t.Fatalf("status = %q, want an aliasing conflict; plan = %v", status, plan)
			}
		}
		if conflicts, _ := resp["conflicts"].(float64); int(conflicts) != 2 {
			t.Fatalf("conflicts = %v, want 2", resp["conflicts"])
		}
		if after := snapshotTree(t, dir); !reflect.DeepEqual(before, after) {
			t.Fatalf("dry run changed disk: before=%v after=%v", before, after)
		}
		requireNoTransactionState(t, transactions)
	})
}

// TestBatchEditDryRunReportsAliasedContentEdits covers the batch the lifecycle
// preflight used to skip entirely: two content edits and no lifecycle item at
// all still abort when their paths name one file, so both entries carry the
// refusal the transaction would raise.
func TestBatchEditDryRunReportsAliasedContentEdits(t *testing.T) {
	repoRoot := t.TempDir()
	transactions := filepath.Join(t.TempDir(), "transactions")
	t.Setenv(batchTransactionDirEnv, transactions)
	s := newReadGuardServer(t, repoRoot)
	source := writeAtomicBatchFixture(t, repoRoot, "a.txt", "source\n")
	alias := filepath.Join(repoRoot, "b.txt")
	if err := os.Link(source, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	before := snapshotTree(t, repoRoot)

	resp, plan := dryRunPlan(t, s, []any{
		map[string]any{"op": "edit_file", "path": source, "old_string": "source", "new_string": "edited"},
		map[string]any{"op": "edit_file", "path": alias, "old_string": "source", "new_string": "changed"},
	})
	for _, entry := range plan {
		if status := planStatus(t, entry); !strings.Contains(status, "name the same file") {
			t.Fatalf("status = %q, want an aliasing conflict; plan = %v", status, plan)
		}
	}
	if conflicts, _ := resp["conflicts"].(float64); int(conflicts) != 2 {
		t.Fatalf("conflicts = %v, want 2", resp["conflicts"])
	}
	if after := snapshotTree(t, repoRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run changed disk: before=%v after=%v", before, after)
	}
	requireNoTransactionState(t, transactions)
}

// TestBatchEditDryRunReportsCaseVariantDestinations covers the pair neither of
// whose paths exists yet: two move destinations one directory apart in nothing
// but the case of their names. Nothing on disk distinguishes them, so the
// preflight has to reach the same refusal the commit does — otherwise the dry
// run reports two planned moves and the commit tears itself apart on the
// second one.
func TestBatchEditDryRunReportsCaseVariantDestinations(t *testing.T) {
	repoRoot := t.TempDir()
	transactions := filepath.Join(t.TempDir(), "transactions")
	t.Setenv(batchTransactionDirEnv, transactions)
	s := newReadGuardServer(t, repoRoot)
	first := writeAtomicBatchFixture(t, repoRoot, "a.txt", "first\n")
	second := writeAtomicBatchFixture(t, repoRoot, "b.txt", "second\n")
	before := snapshotTree(t, repoRoot)

	resp, plan := dryRunPlan(t, s, []any{
		map[string]any{"op": "move_file", "source": first, "destination": filepath.Join(repoRoot, "X.txt")},
		map[string]any{"op": "move_file", "source": second, "destination": filepath.Join(repoRoot, "x.txt")},
	})
	for _, entry := range plan {
		if status := planStatus(t, entry); !strings.Contains(status, "differ only by case") {
			t.Fatalf("status = %q, want a case-variant conflict; plan = %v", status, plan)
		}
	}
	if conflicts, _ := resp["conflicts"].(float64); int(conflicts) != 2 {
		t.Fatalf("conflicts = %v, want 2", resp["conflicts"])
	}
	if after := snapshotTree(t, repoRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run changed disk: before=%v after=%v", before, after)
	}
	requireNoTransactionState(t, transactions)
}

// lifecycleDryRunArgs renders a batch edit as the tool arguments that produce
// it, so one fixture can drive both the dry run and the real run.
func lifecycleDryRunArgs(edit batchEditItem) map[string]any {
	args := map[string]any{"op": edit.Op}
	switch edit.Op {
	case "move_file":
		args["source"], args["destination"] = edit.SourcePath, edit.DestinationPath
	case "edit_file":
		args["path"] = edit.Path
		args["old_string"], args["new_string"] = edit.OldString, edit.NewString
	default:
		args["path"] = edit.Path
	}
	if edit.ExpectedSHA256 != "" {
		args["expected_sha256"] = edit.ExpectedSHA256
	}
	return args
}

// TestBatchEditDryRunConflictsMatchTheRealAbort runs one fixture through both
// paths and requires the dry-run conflict to be spelled exactly like the abort
// the same fixture produces. Wording that drifts is worse than no preflight:
// it teaches an agent a refusal it will never see again.
func TestBatchEditDryRunConflictsMatchTheRealAbort(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the whole batch: a refusal that needs two items to
		// arise — aliasing — is reported on both and aborts on the first.
		setup func(t *testing.T, repoRoot string) []batchEditItem
	}{
		{
			name: "directory source",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				directory := filepath.Join(repoRoot, "directory")
				if err := os.Mkdir(directory, 0o755); err != nil {
					t.Fatal(err)
				}
				return []batchEditItem{atomicFileDelete(directory, "")}
			},
		},
		{
			name: "directory destination",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
				destination := filepath.Join(repoRoot, "directory")
				if err := os.Mkdir(destination, 0o755); err != nil {
					t.Fatal(err)
				}
				return []batchEditItem{atomicFileMove(source, destination, "")}
			},
		},
		{
			name: "symlink source",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				if os.PathSeparator == '\\' {
					t.Skip("symlink creation is not reliably available on Windows CI")
				}
				target := writeAtomicBatchFixture(t, repoRoot, "target.txt", "target\n")
				link := filepath.Join(repoRoot, "link.txt")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				return []batchEditItem{atomicFileDelete(link, "")}
			},
		},
		{
			name: "missing source",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				return []batchEditItem{atomicFileDelete(filepath.Join(repoRoot, "absent.txt"), "")}
			},
		},
		{
			name: "digest mismatch",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
				return []batchEditItem{atomicFileDelete(source, strings.Repeat("0", 64))}
			},
		},
		{
			name: "destination exists",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				source := writeAtomicBatchFixture(t, repoRoot, "source.txt", "source\n")
				destination := writeAtomicBatchFixture(t, repoRoot, "destination.txt", "destination\n")
				return []batchEditItem{atomicFileMove(source, destination, "")}
			},
		},
		{
			name: "case-variant destinations",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				first := writeAtomicBatchFixture(t, repoRoot, "a.txt", "first\n")
				second := writeAtomicBatchFixture(t, repoRoot, "b.txt", "second\n")
				return []batchEditItem{
					atomicFileMove(first, filepath.Join(repoRoot, "X.txt"), ""),
					atomicFileMove(second, filepath.Join(repoRoot, "x.txt"), ""),
				}
			},
		},
		{
			name: "case-variant destinations under a new directory",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				first := writeAtomicBatchFixture(t, repoRoot, "a.txt", "first\n")
				second := writeAtomicBatchFixture(t, repoRoot, "b.txt", "second\n")
				return []batchEditItem{
					atomicFileMove(first, filepath.Join(repoRoot, "newdir", "X.txt"), ""),
					atomicFileMove(second, filepath.Join(repoRoot, "newdir", "x.txt"), ""),
				}
			},
		},
		{
			name: "normalisation-variant destinations",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				first := writeAtomicBatchFixture(t, repoRoot, "a.txt", "first\n")
				second := writeAtomicBatchFixture(t, repoRoot, "b.txt", "second\n")
				return []batchEditItem{
					atomicFileMove(first, filepath.Join(repoRoot, "café.txt"), ""),
					atomicFileMove(second, filepath.Join(repoRoot, "café.txt"), ""),
				}
			},
		},
		{
			name: "symlink leaf and its target",
			setup: func(t *testing.T, repoRoot string) []batchEditItem {
				if os.PathSeparator == '\\' {
					t.Skip("symlink creation is not reliably available on Windows CI")
				}
				target := writeAtomicBatchFixture(t, repoRoot, "a.txt", "source\n")
				link := filepath.Join(repoRoot, "link.txt")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				return []batchEditItem{
					atomicFileEdit(link, "source", "through the link"),
					atomicFileDelete(target, ""),
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			s := newReadGuardServer(t, repoRoot)
			edits := testCase.setup(t, repoRoot)
			before := snapshotTree(t, repoRoot)

			args := make([]any, 0, len(edits))
			for _, edit := range edits {
				args = append(args, lifecycleDryRunArgs(edit))
			}
			_, plan := dryRunPlan(t, s, args)
			if len(plan) != len(edits) {
				t.Fatalf("plan = %v", plan)
			}
			conflicts := make([]string, 0, len(plan))
			for _, entry := range plan {
				status := planStatus(t, entry)
				if conflict := strings.TrimPrefix(status, "conflict: "); conflict != status {
					conflicts = append(conflicts, conflict)
				}
			}
			if len(conflicts) == 0 {
				t.Fatalf("plan = %v, want at least one conflict", plan)
			}

			receipt, err := s.runBatchTransaction(context.Background(), edits, "parity-"+testCase.name)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
				t.Fatalf("receipt = %+v", receipt)
			}
			for _, conflict := range conflicts {
				if conflict != receipt.Error {
					t.Fatalf("dry-run conflict %q does not match the abort %q", conflict, receipt.Error)
				}
			}
			if after := snapshotTree(t, repoRoot); !reflect.DeepEqual(before, after) {
				t.Fatalf("run changed disk: before=%v after=%v", before, after)
			}
		})
	}
}

// TestBatchEditDryRunReportsUnstatableSource pins wording parity for a source
// that cannot be stat-ed at all, which is a different refusal from "does not
// exist": the transaction aborts on the stat error before it can say anything
// about the file, and the dry run has to report that same error rather than
// guess at absence from the failed Lstat.
func TestBatchEditDryRunReportsUnstatableSource(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("directory permission bits do not deny stat on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	repoRoot := t.TempDir()
	t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
	s := newReadGuardServer(t, repoRoot)
	locked := filepath.Join(repoRoot, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	source := writeAtomicBatchFixture(t, locked, "source.txt", "source\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, plan := dryRunPlan(t, s, []any{lifecycleDryRunArgs(atomicFileDelete(source, ""))})
	if len(plan) != 1 {
		t.Fatalf("plan = %v", plan)
	}
	status := planStatus(t, plan[0])
	conflict := strings.TrimPrefix(status, "conflict: ")
	if conflict == status || !strings.Contains(conflict, "could not stat") {
		t.Fatalf("status = %q, want the stat refusal", status)
	}

	receipt, err := s.runBatchTransaction(context.Background(), []batchEditItem{atomicFileDelete(source, "")}, "unstatable-source")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "aborted" || receipt.DiskStatus != "unchanged" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if conflict != receipt.Error {
		t.Fatalf("dry-run conflict %q does not match the abort %q", conflict, receipt.Error)
	}
}
