package indexer

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
)

// The type checker's stage for a generation that describes a committed tree.
//
// A dedicated base, its advance and a commit layer describe a tree, not the
// working copy: their bytes come from git objects. The go/types provider
// loads through the go command, which reads the checkout root on disk, and
// the working copy there may hold edits the tree does not. The stage
// therefore hands the load an overlay that makes the root read as the tree:
// every Go file (and module manifest) the working copy modified or deleted
// reads as its committed content, and every Go file the tree does not hold
// (untracked, ignored, or only added to the index) reads as a file its build
// constraint excludes, so neither it nor a package made only of such files
// takes part in the load.
//
// One difference cannot be overlaid: a module manifest the tree does not hold
// changes which directories belong to which module, and the go command
// offers no way through go/packages to make a file absent. The stage then
// runs nothing and the generation declares the capability incomplete.

// committedTypecheckStage asks a committed tree's build for the stage. Only a
// build over a checkout's own tree sets it; a ref view has no checkout root.
type committedTypecheckStage struct {
	// CheckoutID scopes the pass's log lines and workspace entry.
	CheckoutID string
}

// committedTypesCarried is what a committed tree's build that ran no stage
// of its own carries of the type checker's rows.
type committedTypesCarried struct {
	set      bool
	complete bool
	reason   string
}

// CommittedTypecheckOutcome is what the type checker's stage over a
// committed tree did.
type CommittedTypecheckOutcome struct {
	// Requested reports that the build asked for the stage.
	Requested bool
	// Ran reports that the stage type-checked the whole tree the generation
	// carries.
	Ran bool
	// NothingToCheck reports that the generation carries no Go file, so the
	// stage had nothing to do (Ran is set).
	NothingToCheck bool
	// Preempted reports that an edit's compiler load overtook the stage.
	Preempted bool
	// Reason says why the stage did not run to the end. Empty when it did.
	Reason string
	// Restored and Hidden count the files the overlay read as their
	// committed content and as excluded; OverlayMs is what computing it cost.
	Restored, Hidden int
	OverlayMs        int64
	// Compiler is the load's work, nil when nothing was loaded.
	Compiler *semantic.CompilerLoadStats
}

// runCommittedTypecheck runs the type checker's stage over a committed tree's
// payload. Like the working-tree stage it never fails the build: what it
// could not do, the generation declares.
func (b *SparseGenerationBuilder) runCommittedTypecheck(ctx context.Context, req BuildRequest, handle *store_sqlite.Store, report *BuildReport) {
	out := &report.CommittedTypecheck
	out.Requested = true
	if b.Semantic == nil {
		out.Reason = "no semantic enrichment manager is installed"
		return
	}
	// graph.semantic is the go/types level: a generation that carries no Go
	// file has nothing for the type checker to add, and is whole without it.
	if !carriesGoFiles(handle, req.RepoPrefix) {
		out.Ran, out.NothingToCheck = true, true
		return
	}
	started := time.Now()
	overlay, err := buildCommittedOverlay(ctx, req.RootPath, req.Identity.TreeOID)
	out.OverlayMs = time.Since(started).Milliseconds()
	out.Restored, out.Hidden = overlay.restored, overlay.hidden
	switch {
	case err != nil:
		out.Reason = "the working copy could not be compared with the committed tree: " + err.Error()
		return
	case overlay.refusal != "":
		out.Reason = overlay.refusal
		return
	}
	scope := withCheckoutDeclarations(semantic.CheckoutCompilerScope{
		Committed: true,
		Overlay:   overlay.files,
		GoWorkOff: overlay.goWorkOff,
	}, req.Base)
	pass, err := b.Semantic.EnrichCheckoutContext(ctx, handle, semantic.CheckoutEnrichRequest{
		RepoPrefix: req.RepoPrefix,
		CheckoutID: req.committedTypecheck.CheckoutID,
		Root:       req.RootPath,
		Compiler:   scope,
	})
	out.Compiler = pass.Compiler
	b.Logger.Info("indexer: committed tree's type checker stage",
		zap.String("checkout", req.committedTypecheck.CheckoutID),
		zap.String("tree", req.Identity.TreeOID),
		zap.Int("restored", out.Restored),
		zap.Int("hidden", out.Hidden),
		zap.Int64("overlay_ms", out.OverlayMs),
		zap.Strings("ran", pass.Ran),
		zap.Bool("preempted", pass.Preempted),
		zap.String("reason", pass.Reason),
		zap.Error(err))
	switch {
	case err != nil:
		out.Reason = err.Error()
	case pass.Preempted:
		out.Preempted = true
		out.Reason = pass.Reason
	case pass.Disabled:
		out.Reason = pass.Reason
	case !ranLanguage(pass.Ran, "go"):
		out.Reason = "the type checker did not run over this committed tree: " + enrichmentReason(EnrichmentOutcome{Reason: pass.Reason})
	case pass.Partial:
		out.Reason = "the type checker was cut short before it finished this committed tree"
	default:
		out.Ran = true
	}
}

func ranLanguage(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// hiddenGoFile is what a Go file the committed tree does not hold reads as:
// excluded by its build constraint, so its package clause is never compared
// with its neighbours' and a directory made only of such files is no package.
var hiddenGoFile = []byte("//go:build ignore\n\npackage ignored\n")

// committedOverlay is the overlay one committed pass reads its tree through.
type committedOverlay struct {
	files map[string][]byte
	// restored counts files read as their committed content, hidden files
	// read as excluded.
	restored, hidden int
	// refusal, when set, is why the overlay cannot make the root read as the
	// tree; the stage then runs nothing.
	refusal string
	// goWorkOff reports that the tree holds no go.work at its root: its
	// loads run with GOWORK=off, whatever workspace file the working copy
	// or a directory above it holds.
	goWorkOff bool
}

// goOverlayPath reports whether a repository-relative path is one the go
// command reads for a load: a Go source file or a module manifest.
func goOverlayPath(p string) bool {
	return strings.HasSuffix(p, ".go") || goModuleManifestPath(p)
}

// buildCommittedOverlay compares the checkout at root with treeOID and
// returns the overlay that makes the root read as the tree. Both git reads
// are metadata-only walks of the index and the working copy; neither writes
// the index.
func buildCommittedOverlay(ctx context.Context, root, treeOID string) (committedOverlay, error) {
	out := committedOverlay{files: map[string][]byte{}}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return out, err
	}
	diff, err := gitcmd.RunNoLazy(ctx, root, "diff-index", "--name-status", "--no-renames", "-z", treeOID, "--")
	if err != nil {
		return out, fmt.Errorf("indexer: compare %s with tree %s: %w", root, treeOID, err)
	}
	rootGoWork, err := gitcmd.RunNoLazy(ctx, root, "ls-tree", "--name-only", "-z", treeOID, "--", "go.work")
	if err != nil {
		return out, fmt.Errorf("indexer: list tree %s: %w", treeOID, err)
	}
	out.goWorkOff = strings.TrimRight(string(rootGoWork), "\x00") != "go.work"
	// A go.work the tree does not hold: with GOWORK=off none is read; only
	// one below the root, while the tree holds its own, could still be
	// chosen by a load rooted at a nested module.
	strayGoWork := func(rel string) bool {
		return path.Base(rel) == "go.work" && !out.goWorkOff
	}
	var restore []string
	for _, change := range parseDiffNameStatus(diff) {
		rel := path.Clean(change.Path)
		if !goOverlayPath(rel) {
			continue
		}
		switch change.Status {
		case 'A':
			if path.Base(rel) == "go.work" && !strayGoWork(rel) {
				continue
			}
			if goModuleManifestPath(rel) {
				out.refusal = "the working copy holds a module manifest the committed tree does not: " + rel
				return out, nil
			}
			out.files[filepath.Join(absRoot, filepath.FromSlash(rel))] = hiddenGoFile
			out.hidden++
		default:
			restore = append(restore, rel)
		}
	}
	others, err := gitcmd.RunNoLazy(ctx, root, "ls-files", "--others", "-z", "--", "*.go", "*go.mod", "*go.work")
	if err != nil {
		return out, fmt.Errorf("indexer: list files %s's tree does not hold: %w", root, err)
	}
	for _, rel := range strings.Split(string(others), "\x00") {
		if rel == "" {
			continue
		}
		rel = path.Clean(rel)
		switch {
		case strings.HasSuffix(rel, ".go"):
			out.files[filepath.Join(absRoot, filepath.FromSlash(rel))] = hiddenGoFile
			out.hidden++
		case path.Base(rel) == "go.work" && !strayGoWork(rel):
			// Not read: the tree holds no go.work and the loads run with
			// GOWORK=off.
		case path.Base(rel) == "go.mod" || path.Base(rel) == "go.work":
			// Only a manifest over Go files the tree holds moves them into
			// another module; one in a directory the tree has no Go files
			// under (a tool's cache, a virtual environment) changes nothing.
			holds, err := treeHoldsGoFilesUnder(ctx, root, treeOID, path.Dir(rel))
			if err != nil {
				return out, err
			}
			if holds {
				out.refusal = "the working copy holds a module manifest the committed tree does not: " + rel
				return out, nil
			}
		}
	}
	if len(restore) == 0 {
		return out, nil
	}
	sort.Strings(restore)
	read, closeBatch, err := gitBatchContent(root, treeOID)(ctx)
	if err != nil {
		return out, fmt.Errorf("indexer: read tree %s: %w", treeOID, err)
	}
	defer closeBatch()
	for _, rel := range restore {
		content, ok := read(ctx, rel)
		if !ok {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			return out, fmt.Errorf("indexer: tree %s holds no readable %s", treeOID, rel)
		}
		out.files[filepath.Join(absRoot, filepath.FromSlash(rel))] = content
		out.restored++
	}
	return out, nil
}

// treeHoldsGoFilesUnder reports whether treeOID holds a Go file in dir or
// below it.
func treeHoldsGoFilesUnder(ctx context.Context, root, treeOID, dir string) (bool, error) {
	spec := dir + "/"
	if dir == "." || dir == "" {
		spec = "."
	}
	listed, err := gitcmd.RunNoLazy(ctx, root, "ls-tree", "-r", "--name-only", "-z", treeOID, "--", spec)
	if err != nil {
		return false, fmt.Errorf("indexer: list tree %s under %s: %w", treeOID, dir, err)
	}
	for _, rel := range strings.Split(string(listed), "\x00") {
		if strings.HasSuffix(rel, ".go") {
			return true, nil
		}
	}
	return false, nil
}

// copiedCorpusTypes is what a dedicated base copied from the corpus carries
// of the type checker's rows: complete when the corpus's whole-repository
// enrichment finished at the base's commit, and incomplete otherwise.
func (b *SparseGenerationBuilder) copiedCorpusTypes(repoPrefix, commit string) committedTypesCarried {
	if b.Semantic == nil || b.Store == nil {
		return committedTypesCarried{set: true, reason: "no semantic enrichment manager is installed"}
	}
	current, persisted := b.Semantic.RepoEnrichmentMarkerState(b.Store, repoPrefix, commit)
	if current && persisted {
		return committedTypesCarried{set: true, complete: true}
	}
	return committedTypesCarried{set: true,
		reason: "the corpus the base was copied from had not finished its enrichment at commit " + commit}
}

// carriesGoFiles reports whether the generation holds a Go file of the
// repository.
func carriesGoFiles(handle graph.Store, repoPrefix string) bool {
	for _, row := range graph.ReadRepoLanguageFileCounts(handle, []string{repoPrefix}) {
		if row.Language == "go" && row.Count > 0 {
			return true
		}
	}
	return false
}
