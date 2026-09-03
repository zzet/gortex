package mcp

// Dry-run preflight for the whole-file lifecycle operations.
//
// planBatchTransaction answers "is this batch well-formed"; it never touches
// disk, so a dry run reported "planned" for a move whose source was gone,
// whose digest was stale, or whose destination was already taken. An agent
// that asked before mutating learned nothing it could act on.
//
// The preflight re-runs every precondition of the lifecycle items, plus the
// same-file aliasing rule across all items, through the same functions the
// commit calls — batchPathBytes for the stat-and-read,
// batchLifecycleSourceRefusal for the per-operation checks, and
// batchAliasedPathPairs for the aliasing rule — so a refusal is spelled once
// and reported identically from both. On top of that it adds the evidence a
// decision needs: the resolved absolute paths, the source digest, and how git
// currently sees each end of the operation.
//
// A content edit is otherwise not evaluated against disk: it is inspected for
// aliasing and nothing else, so no read is attempted and no old_string is
// matched. `edit_file` against a directory or an absent path therefore still
// dry-runs as "planned" while the commit aborts on the read.
//
// The preflight is advisory by construction — it takes no mutation path lock,
// creates no journal, and writes nothing — because the real run re-validates
// under the lock. A racing writer can therefore make a preflight answer stale,
// never a commit unsafe. It also answers per item rather than for the batch:
// the transaction aborts on the first refusal, while a dry run reports every
// one, which is the more useful answer and the reason the two agree on wording
// but not on how many refusals they name.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
)

// batchLifecycleGitTimeout bounds one checkout's whole classification, not each
// subprocess: every git invocation made for that checkout shares the deadline,
// the per-path check-ignore retry included, so a wedged repository cannot
// multiply the delay by the number of paths in the batch. Git state is evidence
// attached to an answer, never the answer itself, so a slow or wedged
// repository degrades the classification instead of delaying the dry run. The
// bound holds for git itself; a grandchild that inherits and holds stdout can
// outlive it, because gitcmd sets no WaitDelay.
const batchLifecycleGitTimeout = 3 * time.Second

// gitRecordFormat describes how one git command frames the paths it prints.
type gitRecordFormat struct {
	separator string
	// quoted reports whether git may C-quote a record. Only the newline
	// framing can: `-z` output is verbatim because NUL cannot occur in a path,
	// so a name that is itself spelled like a quoted record — `"q"` — arrives
	// as itself and must not be unquoted back to `q`.
	quoted bool
}

var (
	gitNULRecords     = gitRecordFormat{separator: "\x00"}
	gitNewlineRecords = gitRecordFormat{separator: "\n", quoted: true}
)

// batchPathGit is how git sees one path: its classification plus whether the
// ignore rules cover it, which stays meaningful for a path that does not exist.
type batchPathGit struct {
	classification string // tracked | untracked | ignored | absent | unknown
	ignored        bool
}

// batchPathState is what a dry run reports about one end of a lifecycle op.
type batchPathState struct {
	exists   bool
	kind     string // regular | symlink | directory | other | absent
	fileMode os.FileMode
	sha256   string
	content  []byte // retained for the digest precondition, source ends only
	git      batchPathGit
	refusal  error // what batchPathBytes would abort the transaction with
}

func (state batchPathState) payload() map[string]any {
	out := map[string]any{
		"exists":  state.exists,
		"kind":    state.kind,
		"git":     state.git.classification,
		"ignored": state.git.ignored,
	}
	if state.sha256 != "" {
		out["sha256"] = state.sha256
	}
	return out
}

// batchLifecyclePreflight is one batch item's advisory verdict. A content edit
// gets one only when it aliases another path, which is a batch-level refusal
// rather than anything about the edit itself.
type batchLifecyclePreflight struct {
	source      batchPathState
	destination batchPathState
	conflict    string
}

// annotate writes the preflight onto a dry-run plan entry. Only a lifecycle op
// carries path state, and only a move has a destination, so only a move carries
// the destination state and the absolute path it resolved to. The state objects
// land under `source_state` and `destination_state` because every plan entry
// already carries `destination` as the relative destination path: reusing that
// key would give one key two types across the entries of a single array.
func (preflight *batchLifecyclePreflight) annotate(entry map[string]any, plan plannedBatchEdit) {
	entry["resolved_path"] = plan.absPath
	if isBatchLifecycleOp(plan.op) {
		entry["source_state"] = preflight.source.payload()
		if plan.op == "move_file" {
			entry["resolved_destination"] = plan.destinationPath
			entry["destination_state"] = preflight.destination.payload()
		}
	}
	if preflight.conflict != "" {
		entry["status"] = "conflict: " + preflight.conflict
	}
}

// preflightBatchLifecycle returns a verdict per plan entry, nil for every entry
// with nothing to report. Argument errors keep the "failed: <err>" status the
// plan already reports; a content edit is reported only when it aliases another
// batch path, because that refusal belongs to the batch, not to the edit.
func (s *Server) preflightBatchLifecycle(ctx context.Context, plans []plannedBatchEdit) []*batchLifecyclePreflight {
	preflights := make([]*batchLifecyclePreflight, len(plans))
	lifecycle := false
	for _, plan := range plans {
		if plan.err == "" && isBatchLifecycleOp(plan.op) {
			lifecycle = true
			break
		}
	}

	// Every batch path is inspected once, content edits included: two paths
	// naming one file — or two absent paths one filesystem would create as one
	// — abort the transaction whatever the operations are, and reporting that
	// needs both spellings. A lone path has nothing to be paired against, so a
	// batch without a lifecycle op and without a second path has nothing left
	// to preflight.
	paths, relPaths, infos, followed, statErrs := batchPreflightPaths(plans)
	if !lifecycle && len(paths) < 2 {
		return preflights
	}
	aliases := batchAliasConflicts(paths, relPaths, followed)

	var gitState map[string]batchPathGit
	contents := make(map[string][]byte, len(paths))
	refusals := make(map[string]error, len(paths))
	if lifecycle {
		gitState = s.classifyBatchLifecycleGit(ctx, paths, infos)
		// Bytes are collected for the lifecycle ends only. The transaction
		// reads every batch path, but a read refusal on a content edit's path
		// is already reported by the edit itself, and a dry run has no reason
		// to pull whole files it will not check a precondition against.
		for _, plan := range plans {
			if plan.err != "" || !isBatchLifecycleOp(plan.op) {
				continue
			}
			for _, end := range [2][2]string{{plan.absPath, plan.file}, {plan.destinationPath, plan.destination}} {
				// An absent end has nothing to read; an end whose Lstat failed
				// for any other reason is read anyway, so the stat refusal the
				// transaction raises is reported in the same words.
				if end[0] == "" || (infos[end[0]] == nil && statErrs[end[0]] == nil) {
					continue
				}
				if _, read := refusals[end[0]]; read {
					continue
				}
				_, content, err := batchPathBytes(end[0], end[1])
				contents[end[0]], refusals[end[0]] = content, err
			}
		}
	}

	for i, plan := range plans {
		if plan.err != "" {
			continue
		}
		if !isBatchLifecycleOp(plan.op) {
			if conflict := aliases[plan.absPath]; conflict != "" {
				preflights[i] = &batchLifecyclePreflight{conflict: conflict}
			}
			continue
		}
		preflight := &batchLifecyclePreflight{
			source: batchLifecyclePathState(
				infos[plan.absPath], contents[plan.absPath], refusals[plan.absPath], gitState[plan.absPath], true),
		}
		if plan.op == "move_file" {
			preflight.destination = batchLifecyclePathState(
				infos[plan.destinationPath], nil, refusals[plan.destinationPath], gitState[plan.destinationPath], false)
		}
		preflight.conflict = s.batchLifecycleConflict(ctx, plan, preflight, aliases)
		preflights[i] = preflight
	}
	return preflights
}

// batchPreflightPaths collects every distinct path a well-formed plan entry
// names, in sorted order — which is the order the transaction compares its
// buffers in — together with the relative spelling each refusal quotes, the
// Lstat result behind it, and the identity a symlink leaf ultimately names.
// The transaction collects the same two identities per buffer, which is what
// keeps the aliasing pass answering identically from both.
func batchPreflightPaths(
	plans []plannedBatchEdit,
) ([]string, map[string]string, map[string]os.FileInfo, map[string]os.FileInfo, map[string]error) {
	paths := make([]string, 0, len(plans)*2)
	relPaths := make(map[string]string, len(plans)*2)
	infos := make(map[string]os.FileInfo, len(plans)*2)
	followed := make(map[string]os.FileInfo, len(plans)*2)
	// statErrs keeps every Lstat failure other than absence, which the
	// transaction reports as a refusal of its own rather than as a missing file.
	statErrs := make(map[string]error, len(plans)*2)
	for _, plan := range plans {
		if plan.err != "" {
			continue
		}
		for _, end := range [2][2]string{{plan.absPath, plan.file}, {plan.destinationPath, plan.destination}} {
			if end[0] == "" {
				continue
			}
			if _, seen := relPaths[end[0]]; seen {
				continue
			}
			relPaths[end[0]] = end[1]
			paths = append(paths, end[0])
			if info, err := os.Lstat(end[0]); err == nil {
				infos[end[0]] = info
				followed[end[0]] = batchFollowedIdentity(end[0], info)
			} else if !errors.Is(err, os.ErrNotExist) {
				statErrs[end[0]] = err
			}
		}
	}
	sort.Strings(paths)
	return paths, relPaths, infos, followed, statErrs
}

// batchLifecyclePathState describes one path from what was already collected
// for it. The digest is computed for the source only: it is what an
// expected_sha256 precondition is checked against, and what an agent pins its
// next call to.
func batchLifecyclePathState(
	info os.FileInfo, content []byte, refusal error, git batchPathGit, digest bool,
) batchPathState {
	state := batchPathState{kind: "absent", git: git, refusal: refusal}
	if state.git.classification == "" {
		state.git.classification = "unknown"
	}
	if info == nil {
		return state
	}
	state.exists = true
	state.fileMode = info.Mode()
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		state.kind = "symlink"
	case info.IsDir():
		state.kind = "directory"
	case info.Mode().IsRegular():
		state.kind = "regular"
	default:
		state.kind = "other"
	}
	if digest && state.kind == "regular" && refusal == nil {
		state.content = content
		state.sha256 = digestBatchBytes(content)
	}
	return state
}

// batchLifecycleConflict reports why this operation would not commit, in the
// order the transaction itself refuses. readBatchBuffers walks the plans one at
// a time — source, then the destination guard, then the destination — so a stat
// or read refusal on either end precedes the guard for the next plan; the
// aliasing pass runs once every buffer is collected; and the per-operation
// preconditions run last, when the plan is applied.
func (s *Server) batchLifecycleConflict(
	ctx context.Context, plan plannedBatchEdit, preflight *batchLifecyclePreflight, aliases map[string]string,
) string {
	if preflight.source.refusal != nil {
		return preflight.source.refusal.Error()
	}
	if plan.op == "move_file" {
		if err := s.guardBatchLifecycleDestination(ctx, plan.destinationPath); err != nil {
			return err.Error()
		}
		if preflight.destination.refusal != nil {
			return preflight.destination.refusal.Error()
		}
	}
	if conflict := aliases[plan.absPath]; conflict != "" {
		return conflict
	}
	if plan.op == "move_file" {
		if conflict := aliases[plan.destinationPath]; conflict != "" {
			return conflict
		}
	}
	source := preflight.source
	if err := batchLifecycleSourceRefusal(source.exists, source.fileMode, source.content, plan.edit.ExpectedSHA256); err != nil {
		return err.Error()
	}
	if plan.op == "move_file" && preflight.destination.exists {
		return errBatchDestinationExists.Error()
	}
	return ""
}

// batchAliasConflicts maps every path that names the same file as another
// distinct path in the batch to the refusal the transaction raises for that
// pair, so a dry run reports the wording of the real abort. A path keeps the
// first pairing that names it, which is the pair the transaction would abort
// on when that path is the earlier of the two.
func batchAliasConflicts(
	paths []string, relPaths map[string]string, followed map[string]os.FileInfo,
) map[string]string {
	conflicts := make(map[string]string)
	pairs := batchAliasedPathPairs(paths, func(path string) os.FileInfo { return followed[path] })
	for _, pair := range pairs {
		message := pair.message(relPaths[pair.left], relPaths[pair.right])
		for _, path := range [2]string{pair.left, pair.right} {
			if _, reported := conflicts[path]; !reported {
				conflicts[path] = message
			}
		}
	}
	return conflicts
}

// classifyBatchLifecycleGit reports how git sees every batch path, batched per
// checkout so one repository costs two subprocesses however many paths the
// batch names. The checkout is the same one the destination guard selects,
// falling back to the path's own directory when no checkout claims it.
func (s *Server) classifyBatchLifecycleGit(
	ctx context.Context, paths []string, infos map[string]os.FileInfo,
) map[string]batchPathGit {
	states := make(map[string]batchPathGit, len(paths))
	grouped := make(map[string][]string)
	roots := make([]string, 0, len(paths))
	for _, path := range paths {
		root, _ := s.batchLifecycleRoot(ctx, path)
		if root == "" {
			root = filepath.Dir(path)
		}
		if _, seen := grouped[root]; !seen {
			roots = append(roots, root)
		}
		grouped[root] = append(grouped[root], path)
	}
	for _, root := range roots {
		for path, state := range classifyGitPathsUnderRoot(ctx, root, grouped[root], infos) {
			states[path] = state
		}
	}
	return states
}

// classifyGitPathsUnderRoot classifies one checkout's paths. Classification is
// evidence, never a gate: no git binary, no repository, or a timeout degrades
// the group to "unknown" and the dry run continues. A pathname git refuses
// degrades that path alone — check-ignore rejects the whole invocation over a
// single unusable pathname, so the refusal is re-asked per path and only the
// path with no answer is reported "unknown".
//
// On a case-insensitive filesystem a tracked file addressed by a different
// case is reported "untracked": ls-files echoes the spelling recorded in the
// index, which does not match the spelling that was asked about.
func classifyGitPathsUnderRoot(
	ctx context.Context, root string, paths []string, infos map[string]os.FileInfo,
) map[string]batchPathGit {
	states := make(map[string]batchPathGit, len(paths))
	unknown := func() map[string]batchPathGit {
		for _, path := range paths {
			states[path] = batchPathGit{classification: "unknown"}
		}
		return states
	}
	relPaths := make(map[string]string, len(paths))
	// ls-files takes pathspecs, where a leading ':' is magic: `:(literal)`
	// spells the name as itself. check-ignore takes plain pathnames and
	// rejects the same prefix outright, so only one side carries it.
	pathspecs := make([]string, 0, len(paths))
	names := make([]string, 0, len(paths))
	wanted := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		rel, ok := relativeWithinRoot(root, path)
		if !ok {
			return unknown()
		}
		rel = filepath.ToSlash(rel)
		relPaths[path] = rel
		wanted[rel] = struct{}{}
		pathspecs = append(pathspecs, ":(literal)"+rel)
		names = append(names, rel)
	}
	ctx, cancel := context.WithTimeout(ctx, batchLifecycleGitTimeout)
	defer cancel()
	tracked, err := gitPathSet(ctx, root, gitNULRecords, wanted, false, append([]string{"ls-files", "-z", "--"}, pathspecs...)...)
	if err != nil {
		return unknown()
	}
	ignored, refused, err := gitIgnoredPaths(ctx, root, names, wanted)
	if err != nil {
		return unknown()
	}
	for _, path := range paths {
		rel := relPaths[path]
		if _, unanswered := refused[rel]; unanswered {
			// git will not say whether the ignore rules cover this path, and
			// "tracked but possibly ignored" is not a state the report has, so
			// the path keeps the one honest answer left.
			states[path] = batchPathGit{classification: "unknown"}
			continue
		}
		_, isIgnored := ignored[rel]
		_, isTracked := tracked[rel]
		state := batchPathGit{ignored: isIgnored}
		switch {
		case isTracked:
			state.classification = "tracked"
		case infos[path] == nil:
			// An ignored directory still covers a destination that does not
			// exist yet, so the ignore flag outlives the absent classification.
			state.classification = "absent"
		case isIgnored:
			state.classification = "ignored"
		default:
			state.classification = "untracked"
		}
		states[path] = state
	}
	return states
}

// gitIgnoredPaths reports which of one checkout's names the ignore rules cover,
// plus the names git would not answer for at all. check-ignore rejects the
// whole invocation when a single pathname is unusable — a name that crosses a
// symlinked directory exits 128 with "pathspec … is beyond a symbolic link" —
// so a failed batch is re-asked one name at a time and only the names that fail
// again are reported as refused. An error is returned only when nothing about
// this checkout can be established.
func gitIgnoredPaths(
	ctx context.Context, root string, names []string, wanted map[string]struct{},
) (ignored, refused map[string]struct{}, err error) {
	// check-ignore rejects -z without --stdin, so its records are newline
	// separated; quoting is disabled so a non-ASCII path still comes back in
	// the spelling it was asked about.
	checkIgnore := func(names ...string) (map[string]struct{}, error) {
		return gitPathSet(ctx, root, gitNewlineRecords, wanted, true,
			append([]string{"-c", "core.quotePath=false", "check-ignore", "--"}, names...)...)
	}
	if ignored, err = checkIgnore(names...); err == nil {
		return ignored, nil, nil
	}
	ignored, refused = make(map[string]struct{}), make(map[string]struct{})
	for _, name := range names {
		single, singleErr := checkIgnore(name)
		if singleErr != nil {
			refused[name] = struct{}{}
			continue
		}
		for match := range single {
			ignored[match] = struct{}{}
		}
	}
	if len(refused) == len(names) {
		return nil, nil, err
	}
	return ignored, refused, nil
}

// gitPathSet runs a git command that prints one path per record and returns
// which of the wanted paths it named. Restricting the answer to what was asked
// about is also what keeps a path containing the record separator from being
// mis-read as two: git cannot escape one on a newline-separated stream, and an
// unmatched fragment simply drops out.
//
// emptyOnExitOne accepts `git check-ignore`'s documented "no path matched" exit
// status as an empty answer rather than a failure — it is the ordinary case for
// a batch that touches nothing ignored.
func gitPathSet(
	ctx context.Context, root string, format gitRecordFormat, wanted map[string]struct{},
	emptyOnExitOne bool, args ...string,
) (map[string]struct{}, error) {
	out, err := gitcmd.Run(ctx, root, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if !emptyOnExitOne || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, err
		}
	}
	set := make(map[string]struct{})
	for _, entry := range strings.Split(string(out), format.separator) {
		if format.quoted {
			entry = unquoteGitPath(strings.TrimSuffix(entry, "\r"))
		}
		entry = filepath.ToSlash(entry)
		if _, ok := wanted[entry]; ok {
			set[entry] = struct{}{}
		}
	}
	return set, nil
}

// unquoteGitPath undoes the C-style quoting git applies to a path containing a
// double quote or a backslash. core.quotePath=false suppresses the quoting of
// non-ASCII bytes but not of those two characters, so a record that arrives
// quoted has to be read back before it can be matched against what was asked
// about. Go string literals accept git's escapes — \", \\, \t, \n and \ooo —
// and a record that does not unquote is used as it arrived.
func unquoteGitPath(entry string) string {
	if len(entry) < 2 || !strings.HasPrefix(entry, `"`) || !strings.HasSuffix(entry, `"`) {
		return entry
	}
	unquoted, err := strconv.Unquote(entry)
	if err != nil {
		return entry
	}
	return unquoted
}
