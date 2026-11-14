//go:build windows

package pathguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxLinkHops bounds link expansion. mklink accepts a junction cycle, so
// resolving one would otherwise never end.
const maxLinkHops = 255

// isJunction reports whether info describes a directory junction. ModeIrregular
// alone does not say so: Go also uses it for reparse points that redirect
// nothing, and os.Readlink answers only for symlinks and junctions.
func isJunction(path string, info os.FileInfo) bool {
	if info.Mode()&os.ModeIrregular == 0 {
		return false
	}
	_, err := os.Readlink(path)
	return err == nil
}

func evalLinks(path string) (string, error) {
	// filepath.EvalSymlinks fails on a junction partway along a path, so a
	// success leaves only the last component to check.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		info, err := os.Lstat(resolved)
		if err != nil {
			return "", err
		}
		if !isJunction(resolved, info) {
			return resolved, nil
		}
	}
	current := filepath.Clean(path)
	for range maxLinkHops {
		next, expanded, err := expandFirstLink(current)
		if err != nil {
			return "", err
		}
		if !expanded {
			// No link is left, so this only settles the spelling: letter case
			// and 8.3 short names.
			return filepath.EvalSymlinks(current)
		}
		current = next
	}
	return "", fmt.Errorf("%s: too many links", path)
}

// expandFirstLink replaces the first link along a clean path with its target,
// reporting whether it found one. Every component before that link is a plain
// directory, so the link's lexical parent is its real parent.
func expandFirstLink(path string) (string, bool, error) {
	volume := filepath.VolumeName(path)
	components := strings.Split(path[len(volume):], string(filepath.Separator))
	current := volume
	for i, component := range components {
		if component == "" {
			current += string(filepath.Separator)
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return "", false, err
		}
		if !IsLink(current, info) {
			continue
		}
		target, err := os.Readlink(current)
		if err != nil {
			return "", false, err
		}
		return filepath.Join(linkDestination(current, target), filepath.Join(components[i+1:]...)), true, nil
	}
	return path, false, nil
}

// linkDestination places a link's target as os.Readlink returns it, which may
// be absolute, rooted on the link's volume, or relative to the link.
func linkDestination(link, target string) string {
	switch {
	case filepath.VolumeName(target) != "":
		return target
	case target != "" && os.IsPathSeparator(target[0]):
		return filepath.VolumeName(link) + target
	default:
		return filepath.Join(filepath.Dir(link), target)
	}
}
