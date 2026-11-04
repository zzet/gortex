package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/pathkey"
)

// pendingTextRoot owns one anchor and, for the serial scanner, a bounded
// O(depth) ancestor stack. It shares descriptors, never file-version or path
// authority. Results stay provisional until all borrowed directories and the
// named root are revalidated and closed.
type pendingTextRoot struct {
	root, canonical string
	handle          *os.Root
	info            os.FileInfo
	resolve         func(string) (string, error)
	lstat           func(*os.Root, string) (os.FileInfo, error)
	openRoot        func(*os.Root, string) (*os.Root, error)
	grouped         bool
	directories     []pendingTextDirectory
	groupErr        error
}

type pendingTextPathStamp struct {
	path string
	info os.FileInfo
}

func openPendingTextRoot(root string) (*pendingTextRoot, error) {
	return openPendingTextRootResolved(root, filepath.EvalSymlinks)
}

// The serial scanner borrows a bounded ancestor stack for one provisional
// result group. Standalone reads keep their original descriptor lifetime.
func openPendingTextRootForScan(root string) (*pendingTextRoot, error) {
	reader, err := openPendingTextRoot(root)
	if err == nil {
		reader.grouped = true
	}
	return reader, err
}

func openPendingTextRootResolved(root string, resolve func(string) (string, error)) (*pendingTextRoot, error) {
	canonical, err := resolve(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || !os.SameFile(info, info) {
		return nil, errPhysicalFileMoved
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	r := &pendingTextRoot{root: root, canonical: filepath.Clean(canonical), handle: handle, info: info, resolve: resolve, lstat: (*os.Root).Lstat, openRoot: (*os.Root).OpenRoot}
	if err := r.validate(); err != nil {
		_ = handle.Close()
		return nil, err
	}
	return r, nil
}

func (r *pendingTextRoot) validate() error {
	// Freeze identities while obtained (including lazy Windows file IDs).
	for _, path := range []string{r.root, r.canonical} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || !os.SameFile(r.info, info) {
			return errPhysicalFileMoved
		}
	}
	opened, err := r.handle.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(r.info, opened) {
		return errPhysicalFileMoved
	}
	return nil
}

func (r *pendingTextRoot) close(ctx context.Context) error {
	// Always validate, including empty/missing/canceled/partially scanned groups.
	// A failure must not allow a complete empty or partial result to escape.
	var directoriesErr error
	if r.grouped {
		directoriesErr = r.closeDirectories(0)
	}
	err := r.validate()
	canonical, resolveErr := r.resolve(r.root)
	if resolveErr == nil && filepath.Clean(canonical) != r.canonical {
		resolveErr = errPhysicalFileMoved
	}
	return errors.Join(r.groupErr, directoriesErr, err, resolveErr, ctx.Err(), r.handle.Close())
}

// walk checks each relative component before/after I/O. Standalone reads keep
// only the current child root; the serial scanner borrows an ancestor stack.
// expected is the immutable before-I/O identity proof for the second walk.
func (r *pendingTextRoot) walk(ctx context.Context, relative string, expected []pendingTextPathStamp) (stamps []pendingTextPathStamp, parent *os.Root, legacy bool, err error) {
	if r.grouped {
		return r.walkGrouped(ctx, relative, expected)
	}
	dir := r.handle
	defer func() {
		if parent == nil && dir != r.handle {
			_ = dir.Close()
		}
	}()
	parts := strings.Split(relative, string(filepath.Separator))
	if expected != nil && len(expected) != len(parts) {
		return nil, nil, false, errPhysicalFileMoved
	}
	path := ""
	for i, part := range parts {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		path = filepath.Join(path, part)
		info, err := r.lstat(dir, part)
		if err != nil {
			return nil, nil, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if expected != nil {
				return nil, nil, false, errPhysicalFileMoved
			}
			return nil, nil, true, nil
		}
		if !os.SameFile(info, info) {
			return nil, nil, false, errPhysicalFileMoved
		}
		if expected != nil && (i >= len(expected) || expected[i].path != path || !os.SameFile(expected[i].info, info)) {
			return nil, nil, false, errPhysicalFileMoved
		}
		stamps = append(stamps, pendingTextPathStamp{path: path, info: info})
		if i == len(parts)-1 {
			return stamps, dir, false, nil
		}
		if !info.IsDir() {
			return nil, nil, false, errPhysicalFileMoved
		}
		next, err := r.openRoot(dir, part)
		if err != nil {
			return nil, nil, false, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = next.Close()
			if err != nil {
				return nil, nil, false, err
			}
			return nil, nil, false, errPhysicalFileMoved
		}
		if dir != r.handle {
			_ = dir.Close()
		}
		dir = next
	}
	return nil, nil, false, errPhysicalFileMoved
}

// mutationKey shares only the already bound absolute prefix. The grouped
// descriptor walk proves every parent is a real directory; the leaf is never
// followed, matching MutationKey's writer policy. Alias/missing-parent cases
// retain the original resolver. All keys remain provisional until close.
func (r *pendingTextRoot) mutationKey(ctx context.Context, relative string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	relative = filepath.Clean(filepath.FromSlash(relative))
	if !filepath.IsLocal(relative) || relative == "." {
		return "", fmt.Errorf("pending text path is not a local file")
	}
	absolute := filepath.Join(r.root, relative)
	if !r.grouped {
		return pathkey.MutationKey(absolute), ctx.Err()
	}
	_, _, _, err := r.walk(ctx, relative, nil)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// walkGrouped retains every non-symlink parent before visiting the leaf,
	// including a missing or symlink leaf. A short stack means a parent alias
	// or absent suffix still needs the established canonicalization behavior.
	parents := strings.Count(relative, string(filepath.Separator))
	if len(r.directories) == parents {
		return pathkey.IdentityKey(filepath.Join(r.canonical, relative)), ctx.Err()
	}
	return pathkey.MutationKey(absolute), ctx.Err()
}

func (r *pendingTextRoot) read(ctx context.Context, relative string, afterRead func(), maxBytes int64, scratch []byte) (raw []byte, evidence physicalReadEvidence, err error) {
	defer func() {
		if r.grouped && err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = r.refuseGroup(err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, physicalReadEvidence{}, err
	}
	relative = filepath.Clean(filepath.FromSlash(relative))
	if !filepath.IsLocal(relative) || relative == "." {
		return nil, physicalReadEvidence{}, fmt.Errorf("pending text path is not a local file")
	}
	// Capture one coherent file version before a later tool write. Keep all
	// physical hashes and path/root checks: writers outside the tool protocol
	// still have to be detected, rather than lending authority to mixed bytes.
	key := pathkey.MutationKey(filepath.Join(r.root, relative))
	if !mutationPathAdmitted(ctx, key) {
		release, err := acquireMutationPathKey(ctx, key)
		if err != nil {
			return nil, physicalReadEvidence{}, err
		}
		defer release()
	}
	stamps, parent, legacy, err := r.walk(ctx, relative, nil)
	if err != nil {
		return nil, physicalReadEvidence{}, err
	}
	if legacy {
		// Preserve the established symlink behavior and full public-path checks.
		raw, evidence, err := readPhysicalFileEvidenceObservedBoundedBuffer(filepath.Join(r.root, relative), afterRead, maxBytes, scratch)
		if r.grouped {
			return raw, evidence, errors.Join(err, ctx.Err())
		}
		return raw, evidence, errors.Join(err, r.validate(), ctx.Err())
	}
	defer func() {
		if !r.grouped && parent != nil && parent != r.handle {
			_ = parent.Close()
		}
	}()
	leaf := stamps[len(stamps)-1]
	if !leaf.info.Mode().IsRegular() {
		return nil, physicalReadEvidence{}, fmt.Errorf("physical evidence requires a regular file, got %s", leaf.info.Mode().Type())
	}
	f, err := openRootPhysicalEvidenceFile(parent, filepath.Base(relative))
	if !r.grouped && parent != r.handle {
		_ = parent.Close()
		parent = nil
	}
	if err != nil {
		return nil, physicalReadEvidence{}, err
	}
	defer f.Close()
	raw, evidence, after, err := readPhysicalEvidenceDescriptor(f, afterRead, maxBytes, scratch)
	if err != nil {
		return nil, physicalReadEvidence{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, physicalReadEvidence{}, err
	}
	checked, afterParent, _, err := r.walk(ctx, relative, stamps)
	if !r.grouped && afterParent != nil && afterParent != r.handle {
		_ = afterParent.Close()
	}
	if err != nil {
		return nil, physicalReadEvidence{}, err
	}
	if !samePhysicalFileVersion(leaf.info, after) || !samePhysicalFileVersion(after, checked[len(checked)-1].info) {
		return nil, physicalReadEvidence{}, errPhysicalFileMoved
	}
	if err := r.validate(); err != nil {
		return nil, physicalReadEvidence{}, err
	}
	evidence.resolvedPath = filepath.Join(r.canonical, relative)
	return raw, evidence, nil
}
