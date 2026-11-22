package indexer

import (
	"path"
	"strings"
)

// cgroupV2MemoryLimit reads limits only along the process's mounted unified
// hierarchy. A child can report "max" while its visible parent is bounded.
func cgroupV2MemoryLimit(readFile func(string) ([]byte, error)) uint64 {
	membership, err := readFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	group := unifiedCgroupPath(string(membership))
	if group == "" {
		return 0
	}

	mounts, err := readFile("/proc/self/mountinfo")
	if err != nil {
		return 0
	}
	var limit uint64
	for line := range strings.SplitSeq(string(mounts), "\n") {
		mount, current, ok := mountedCgroupPath(line, group)
		if !ok {
			continue
		}
		for {
			if next, ok := parseCgroupMemoryLimit(readFile, path.Join(current, "memory.max")); ok && (limit == 0 || next < limit) {
				limit = next
			}
			if current == mount {
				break
			}
			current = path.Dir(current)
		}
	}
	return limit
}

func unifiedCgroupPath(data string) string {
	var group string
	for line := range strings.SplitSeq(data, "\n") {
		current, unified := strings.CutPrefix(line, "0::")
		if !unified {
			continue
		}
		if group != "" || !cleanCgroupPath(current) {
			return ""
		}
		group = current
	}
	return group
}

func mountedCgroupPath(line, group string) (mount, current string, ok bool) {
	left, right, found := strings.Cut(line, " - ")
	if !found {
		return "", "", false
	}
	// Kernel records separate fields with ASCII spaces. A literal Unicode
	// space can be part of a pathname.
	fields := strings.FieldsFunc(left, mountInfoSpace)
	filesystem := strings.FieldsFunc(right, mountInfoSpace)
	if len(fields) < 6 || len(filesystem) != 3 || filesystem[0] != "cgroup2" {
		return "", "", false
	}
	root, mount := mountInfoCgroupPath(fields[3]), mountInfoCgroupPath(fields[4])
	if root == "" || mount == "" {
		return "", "", false
	}
	if group == root {
		return mount, mount, true
	}
	// A delegated mount exposes only paths below its own hierarchy root.
	suffix, ok := strings.CutPrefix(group, strings.TrimSuffix(root, "/")+"/")
	if !ok {
		return "", "", false
	}
	return mount, path.Join(mount, suffix), true
}

func mountInfoSpace(r rune) bool { return r == ' ' }

func cleanCgroupPath(name string) bool {
	return path.IsAbs(name) && path.Clean(name) == name && !strings.ContainsRune(name, 0)
}

// mountinfo uses octal escapes for whitespace and literal backslashes in its
// pathname fields. Decode only those four escapes before checking confinement.
func mountInfoCgroupPath(raw string) string {
	var decoded strings.Builder
	for len(raw) != 0 {
		prefix, rest, escaped := strings.Cut(raw, `\`)
		decoded.WriteString(prefix)
		if !escaped {
			break
		}
		if len(rest) < 3 {
			return ""
		}
		switch rest[:3] {
		case "040":
			decoded.WriteByte(' ')
		case "011":
			decoded.WriteByte('\t')
		case "012":
			decoded.WriteByte('\n')
		case "134":
			decoded.WriteByte('\\')
		default:
			return ""
		}
		raw = rest[3:]
	}
	name := decoded.String()
	if !cleanCgroupPath(name) {
		return ""
	}
	return name
}
