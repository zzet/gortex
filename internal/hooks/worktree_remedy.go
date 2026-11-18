package hooks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/toolref"
)

// The read and search doors redirect to graph tools, and those tools answer
// out of the view the session CWD selects. A target inside a linked worktree
// the session is not working in is served by a different view: following the
// standard "call explore, then read" remedy resolves against the session's
// own checkout, where a branch-new file is invisible and a modified one comes
// back with the wrong bytes. These helpers let the deny and advisory text name
// the view the target actually lives in (#820).

// gitPointerMaxBytes bounds how much of a `.git` file is read. A linked
// worktree's pointer is one `gitdir:` line; anything larger is not one.
const gitPointerMaxBytes = 4096

// linkedWorktreeRoot returns the root of the linked git worktree containing
// abs, walking up from abs's directory to the nearest `.git` entry. A `.git`
// directory is an ordinary checkout and a `.git` file pointing anywhere but
// into a `worktrees/` admin directory is a submodule; both answer "".
//
// Deliberately symlink-naive, matching hookAbsScopePath and repoRootForFile.
func linkedWorktreeRoot(abs string) string {
	if abs == "" || !filepath.IsAbs(abs) {
		return ""
	}
	dir := filepath.Dir(filepath.Clean(abs))
	for {
		info, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			if info.IsDir() || !info.Mode().IsRegular() {
				return ""
			}
			if gitPointerIsLinkedWorktree(filepath.Join(dir, ".git")) {
				return dir
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// gitPointerIsLinkedWorktree reports whether the `.git` file at path is a
// linked worktree's pointer (`gitdir: <common>/worktrees/<name>`).
func gitPointerIsLinkedWorktree(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf, err := io.ReadAll(io.LimitReader(f, gitPointerMaxBytes))
	if err != nil {
		return false
	}
	line, _, _ := strings.Cut(string(buf), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return false
	}
	gitdir = filepath.ToSlash(strings.TrimSpace(gitdir))
	return strings.Contains(gitdir, "/worktrees/")
}

// foreignWorktreeRoot returns the linked worktree root that target lives in
// when the session CWD is outside it — the one case where graph tools called
// without a view selector answer from a different working copy than the file
// the agent named. "" means the standard remedy holds: target is not in a
// linked worktree, its path cannot be placed, or the session already works in
// that worktree.
func foreignWorktreeRoot(cwd, target string) string {
	abs, ok := hookAbsScopePath(target, cwd)
	if !ok {
		return ""
	}
	root := linkedWorktreeRoot(abs)
	if root == "" {
		return ""
	}
	if cwdAbs, ok := hookAbsScopePath(cwd, ""); ok && hasPathPrefix(cwdAbs, root) {
		return ""
	}
	return root
}

// worktreeViewSelector is the explicit view selection for an automatic
// worktree overlay, in the shape the `view` parameter documents.
func worktreeViewSelector(root string) string {
	return fmt.Sprintf("view:{kind:\"worktree\",path:%q}", root)
}

// worktreeViewNote explains why the target needs an explicit view and how to
// tell whether that view was actually served.
func worktreeViewNote(root string, view *daemon.ProbeView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  This file is in the linked worktree %s, not the session's checkout. Graph tools default to the session CWD's view, where this worktree's files are missing or stale — pass `%s` on each call.\n", root, worktreeViewSelector(root))
	if view != nil && !view.Exact {
		reason := view.FallbackReason
		if reason == "" {
			reason = "fallback"
		}
		fmt.Fprintf(&b, "  The worktree's own view is not being served right now (%s): responses will carry `exact:false`. Pass `require_exact:true` to refuse substitute content.\n", reason)
	} else {
		b.WriteString("  A response with freshness `exact:false` means the worktree's view was not served.\n")
	}
	return b.String()
}

// worktreeReadDeny is the Read deny for an indexed file in a foreign linked
// worktree: the same redirect, with every call carrying the view selector.
func worktreeReadDeny(filePath, root string, st fileIndexStatus) string {
	sel := worktreeViewSelector(root)
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: Read of %s (%d symbols indexed). Call `explore` first, then use `read` instead — both with the worktree's view:\n", filePath, st.Count)
	b.WriteString(worktreeViewNote(root, st.View))
	fmt.Fprintf(&b, "  - `read(target:{symbol:\"<id>\"}, %s)` — one symbol\n", sel)
	fmt.Fprintf(&b, "  - `read(target:{symbols:[\"<id>\"]}, %s)` — several symbols\n", sel)
	fmt.Fprintf(&b, "  - `read(operation:\"editing_context\", target:{file:\"<path>\"}, %s)` — full editing context\n", sel)
	b.WriteString(gcxTip)
	b.WriteString(toolref.MCPRequiredLine())
	return b.String()
}

// worktreeReadAdvisory replaces the "use explore first" advisory for a file in
// a foreign linked worktree that its view does not hold yet — typically a
// branch-new file, or a worktree whose view is still being built. The native
// Read proceeds; the advice is what to do once the view answers.
func worktreeReadAdvisory(filePath, root string, st fileIndexStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] %s is not in the graph yet; this Read proceeds.\n", filePath)
	b.WriteString(worktreeViewNote(root, st.View))
	fmt.Fprintf(&b, "  - once indexed: `read(operation:\"summary\", target:{file:\"<path>\"}, %s)`\n", worktreeViewSelector(root))
	b.WriteString(toolref.MCPRequiredLine())
	return b.String()
}

// worktreeSearchDeny is formatTrackedSearchDeny for a search scoped into a
// foreign linked worktree: the indexed search surface, with the view selector.
func worktreeSearchDeny(tool, query, root string) string {
	sel := worktreeViewSelector(root)
	var b strings.Builder
	fmt.Fprintf(&b, "[Gortex] BLOCKED: %s `%s` targets indexed source. Use the indexed search surface instead, with the worktree's view:\n", tool, query)
	b.WriteString(worktreeViewNote(root, nil))
	fmt.Fprintf(&b, "  - `search(operation:\"text\", query:\"<literal>\", %s)` — literal or regex-related source lookup\n", sel)
	fmt.Fprintf(&b, "  - `search(operation:\"symbols\", query:\"<name>\", %s)` — symbol lookup\n", sel)
	b.WriteString(gcxTip)
	b.WriteString(toolref.MCPRequiredLine())
	return b.String()
}
