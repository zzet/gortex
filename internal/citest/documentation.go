package citest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func ordinaryDocumentationPath(name string) bool {
	if !strings.HasPrefix(name, "docs/") || !strings.HasSuffix(name, ".md") || strings.ContainsAny(name, "\\\x00\t\r\n:") || filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// Git's mode remains authoritative on platforms checking out tracked symlinks
// as ordinary files. Literal pathspecs and the exact returned name prevent a
// wildcard-shaped name from qualifying some other checkout entry.
func (p *Planner) documentationInputs(ctx context.Context, head string, changes []Change) (set, error) {
	verified := set{}
	for _, change := range changes {
		name := change.Paths[0] // Plan has already checked ordinaryChange.
		if !ordinaryDocumentationPath(name) {
			continue
		}
		out, err := p.exec(ctx, p.Env, "git", "--literal-pathspecs", "ls-tree", "-z", head, "--", name)
		if err != nil {
			return nil, err
		}
		header, exact := strings.CutSuffix(out, "\t"+name+"\x00")
		fields := strings.Fields(header)
		if !exact || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" || !commitSHA.MatchString(fields[2]) {
			return nil, fmt.Errorf("documentation is not an ordinary tracked blob: %s", name)
		}
		verified.add(name)
	}
	return verified, nil
}

// Only docs outside known or uncertain Go package/module ownership may be
// omitted. This checks the changed file's ancestors, never scans the source
// tree or tries to infer arbitrary runtime file reads from Go source.
func (g Graph) validateDocumentation(root, name string) error {
	if !g.documentation.has(name) || g.packageDirs == nil || len(g.owners[name]) != 0 {
		return fmt.Errorf("documentation has unknown, embedded or build input ownership: %s", name)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	var checked []struct {
		path string
		info os.FileInfo
	}
	parts := strings.Split(name, "/")
	for i := 0; i < len(parts); i++ {
		rel := filepath.Join(parts[:i]...)
		if rel == "" {
			rel = "."
		}
		dir := filepath.Join(root, rel)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || g.packageDirs.has(filepath.ToSlash(rel)) {
			return fmt.Errorf("documentation has unsupported ancestor/package ownership: %s", name)
		}
		checked = append(checked, struct {
			path string
			info os.FileInfo
		}{dir, info})
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			input := entry.Name()
			if strings.HasSuffix(input, ".go") || i > 0 && (input == "go.mod" || input == "go.sum" || input == "go.work" || input == "go.work.sum") {
				return fmt.Errorf("documentation has uncertain package/module inputs: %s", name)
			}
		}
	}
	file := filepath.Join(root, filepath.FromSlash(name))
	before, err := os.Lstat(file)
	if err != nil || !before.Mode().IsRegular() {
		return fmt.Errorf("documentation is not a regular checkout file: %s", name)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	opened, statErr := f.Stat()
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if statErr != nil || readErr != nil || closeErr != nil || !os.SameFile(before, opened) || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("documentation changed identity or is not valid UTF-8: %s", name)
	}
	checked = append(checked, struct {
		path string
		info os.FileInfo
	}{file, before})
	for _, entry := range checked {
		after, err := os.Lstat(entry.path)
		if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(entry.info, after) {
			return fmt.Errorf("documentation changed checkout identity: %s", name)
		}
	}
	return nil
}
