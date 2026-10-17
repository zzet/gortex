package indexer

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/modules"
)

// workspaceMembershipIndex implements resolver.WorkspaceMembership: it
// maps a graph file path to the package-manager workspace member it
// belongs to.
//
// A monorepo is often a package-manager workspace — a root manifest
// (npm/yarn package.json, pnpm-workspace.yaml, Cargo.toml) that owns a
// set of member packages. Two member packages can each declare a
// package or symbol of the same name; when import resolution hits such
// a collision the resolver prefers the candidate whose file shares the
// importing file's member package. The identifier returned per file is
// the member package's repo-prefixed directory — files in the same
// member compare equal, files in different members compare unequal,
// and files outside every member directory return "".
//
// The index is built once at construction (one workspace detection +
// member-glob expansion per repo) and is read-only afterwards, so the
// lookup is allocation-free and safe for the resolver's parallel
// resolveEdge workers without locking.
type workspaceMembershipIndex struct {
	// memberDirs holds, per repo, the repo-relative directories that
	// are workspace members, sorted longest-first so the most-specific
	// enclosing member wins for a nested layout.
	memberDirs map[string][]string
}

// newWorkspaceMembershipIndex detects the package-manager workspace at
// each repo root and records its resolved member directories. Returns
// nil when no repo is a workspace root — callers treat a nil lookup as
// "no workspace signal", the pre-feature behaviour.
func newWorkspaceMembershipIndex(roots map[string]string) *workspaceMembershipIndex {
	memberDirs := map[string][]string{}
	for prefix, root := range roots {
		if root == "" {
			continue
		}
		manifest := modules.DetectWorkspace(root)
		if manifest == nil {
			continue
		}
		members := modules.ResolveWorkspaceMembers(root, manifest)
		if len(members) == 0 {
			continue
		}
		dirs := make([]string, 0, len(members))
		for _, m := range members {
			dirs = append(dirs, m.Dir)
		}
		// Longest-first: a member nested inside another member dir
		// must be matched before its ancestor.
		sort.Slice(dirs, func(i, j int) bool {
			return len(dirs[i]) > len(dirs[j])
		})
		memberDirs[prefix] = dirs
	}
	if len(memberDirs) == 0 {
		return nil
	}
	return &workspaceMembershipIndex{memberDirs: memberDirs}
}

// Lookup is the resolver.WorkspaceMembership entry point. filePath is a
// graph file path — repo-prefixed in multi-repo mode, bare in
// single-repo mode. It returns the repo-prefixed directory of the
// workspace member that owns the file, or "" when the file belongs to
// no workspace member.
func (x *workspaceMembershipIndex) Lookup(filePath string) string {
	if x == nil || filePath == "" {
		return ""
	}
	filePath = path.Clean(filePath)
	// Try every repo: an entry with an empty prefix is single-repo
	// mode (graph paths carry no prefix); a non-empty prefix must be a
	// path ancestor of the file.
	for prefix, dirs := range x.memberDirs {
		rel := filePath
		if prefix != "" {
			if filePath != prefix && !strings.HasPrefix(filePath, prefix+"/") {
				continue
			}
			rel = strings.TrimPrefix(filePath, prefix+"/")
		}
		for _, dir := range dirs {
			if rel == dir || strings.HasPrefix(rel, dir+"/") {
				// Identifier is the repo-prefixed member dir so two
				// repos with an identically-named member directory do
				// not collide.
				if prefix == "" {
					return dir
				}
				return prefix + "/" + dir
			}
		}
	}
	return ""
}

// indexerWorkspaceMembership lazily builds this Indexer's workspace
// membership index and resolves a file path through it. Installed on
// the resolver via SetWorkspaceMembership. Lazy because the repo root
// and prefix are not final until after New().
func (idx *Indexer) indexerWorkspaceMembership(filePath string) string {
	idx.workspaceMembersOnce.Do(func() {
		if proof := idx.contentProof; proof.active() && idx.rootPath != "" {
			// Detection reads the root's workspace manifests off the
			// working copy, and a workspace root's member globs read
			// directories no read set holds: a build that proves its reads
			// claims the manifests, and is fenced by a full sample when the
			// root is a workspace.
			for _, name := range workspaceRootManifests {
				proof.record(filepath.Join(idx.rootPath, name), "")
			}
			if modules.DetectWorkspace(idx.rootPath) != nil {
				proof.noteUnboundedReader()
			}
		}
		idx.workspaceMembers = newWorkspaceMembershipIndex(
			map[string]string{idx.repoPrefix: idx.rootPath})
	})
	return idx.workspaceMembers.Lookup(filePath)
}

// workspaceRootManifests are the root manifests modules.DetectWorkspace reads
// to decide whether a repository is a package-manager workspace root.
var workspaceRootManifests = []string{"package.json", "pnpm-workspace.yaml", "Cargo.toml"}
