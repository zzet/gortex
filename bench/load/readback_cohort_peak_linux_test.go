//go:build acceptance && linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// This observer holds the unit directory FD across offers. It does not create,
// reset, move or delegate a cgroup. Unit creation/ownership is independently
// checked against the existing outer's systemd and normal-closure evidence.
type linuxCohortPeakObserver struct {
	dir      *os.File
	group    string
	identity cohortFileIdentity
	boot     string
	reason   string
}

func boundedCohortRead(f *os.File, maximum int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maximum {
		return nil, errors.New("cohort evidence exceeds bound")
	}
	return b, nil
}

func cohortReadPath(path string, maximum int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	b, readErr := boundedCohortRead(f, maximum)
	return string(b), errors.Join(readErr, f.Close())
}

func cohortIdentity(f *os.File) (cohortFileIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return cohortFileIdentity{}, err
	}
	raw, err := cohortReadPath(fmt.Sprintf("/proc/self/fdinfo/%d", f.Fd()), 4096)
	if err != nil {
		return cohortFileIdentity{}, err
	}
	var mount uint64
	for _, line := range strings.Split(raw, "\n") {
		if value, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			if mount != 0 {
				return cohortFileIdentity{}, errors.New("duplicate FD mount identity")
			}
			mount, err = strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return cohortFileIdentity{}, err
			}
		}
	}
	if mount == 0 || st.Ino == 0 {
		return cohortFileIdentity{}, errors.New("FD identity unavailable")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return cohortFileIdentity{}, err
	}
	return cohortFileIdentity{Device: uint64(st.Dev), Inode: st.Ino, MountID: mount, FSType: uint64(fs.Type)}, nil
}

func cohortUnitIdentity(f *os.File) (cohortFileIdentity, error) {
	identity, err := cohortIdentity(f)
	if err != nil {
		return cohortFileIdentity{}, err
	}
	if identity.FSType != unix.CGROUP2_SUPER_MAGIC {
		return cohortFileIdentity{}, errors.New("held unit directory is not cgroup v2")
	}
	return identity, nil
}

func cohortOpenAt(parent *os.File, name string, directory bool) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return nil, errors.New("invalid cgroup child")
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func cohortGroup(raw string) (string, error) {
	group := ""
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if value, ok := strings.CutPrefix(line, "0::"); ok {
			if group != "" || value == "" || !strings.HasPrefix(value, "/") || filepath.Clean(value) != value {
				return "", errors.New("ambiguous/invalid cgroup v2 membership")
			}
			group = value
		}
	}
	if group == "" {
		return "", errors.New("cgroup v2 membership missing")
	}
	return group, nil
}

func newCohortPeakObserver() cohortPeakObserver {
	out := &linuxCohortPeakObserver{}
	raw, err := cohortReadPath("/proc/self/cgroup", 4096)
	if err == nil {
		out.group, err = cohortGroup(raw)
	}
	if err == nil && (filepath.Dir(out.group) != "/system.slice" || !strings.HasSuffix(filepath.Base(out.group), ".service")) {
		err = errors.New("not a reference unit leaf")
	}
	var root *os.File
	if err == nil {
		root, err = os.Open("/sys/fs/cgroup")
	}
	if root != nil {
		defer func() { _ = root.Close() }()
		var fs unix.Statfs_t
		if err == nil {
			err = unix.Fstatfs(int(root.Fd()), &fs)
		}
		if err == nil && fs.Type != unix.CGROUP2_SUPER_MAGIC {
			err = errors.New("not a cgroup v2 mount")
		}
		if err == nil {
			var slice *os.File
			slice, err = cohortOpenAt(root, "system.slice", true)
			if err == nil {
				out.dir, err = cohortOpenAt(slice, filepath.Base(out.group), true)
				err = errors.Join(err, slice.Close())
			}
		}
	}
	if err == nil {
		out.identity, err = cohortUnitIdentity(out.dir)
	}
	if err == nil {
		out.boot, err = cohortReadPath("/proc/sys/kernel/random/boot_id", 128)
		out.boot = strings.TrimSpace(out.boot)
	}
	if err == nil && out.boot == "" {
		err = errors.New("boot epoch unavailable")
	}
	if err != nil {
		out.reason = err.Error()
	}
	return out
}

func (o *linuxCohortPeakObserver) close() error {
	if o.dir == nil {
		return nil
	}
	err := o.dir.Close()
	o.dir = nil
	return err
}

func (o *linuxCohortPeakObserver) observe(ctx context.Context, phase string, daemonPID int) (out cohortPeakBookend) {
	out = cohortPeakBookend{Version: 1, Phase: phase, StartedAt: time.Now(), Scope: "fresh_owned_reference_unit_lifetime_memory_peak", Unit: filepath.Base(o.group), Group: o.group, BootID: o.boot, DriverPID: os.Getpid(), DaemonPID: daemonPID, Cgroup: o.identity}
	defer func() { out.ObservedAt = time.Now() }()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	fail := func(err error) cohortPeakBookend {
		out.PeakBytes = nil
		out.UnavailableReason = err.Error()
		return out
	}
	if o.reason != "" {
		return fail(errors.New(o.reason))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	var mono unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return fail(err)
	}
	if mono.Sec < 0 || mono.Nsec < 0 {
		return fail(errors.New("monotonic epoch unavailable"))
	}
	out.MonotonicNS = uint64(mono.Sec)*1000000000 + uint64(mono.Nsec)
	// Reopening the named directory detects removal/recreation while the original
	// directory remains held. The peak itself is read relative to the held FD.
	current, err := os.Open(filepath.Join("/sys/fs/cgroup", o.group))
	if err != nil {
		return fail(err)
	}
	identity, identityErr := cohortUnitIdentity(current)
	err = errors.Join(identityErr, current.Close())
	if err != nil {
		return fail(err)
	}
	if identity != o.identity {
		return fail(errors.New("unit cgroup directory replaced"))
	}
	out.SelfCgroup, err = cohortReadPath("/proc/self/cgroup", 4096)
	if err != nil {
		return fail(err)
	}
	group, err := cohortGroup(out.SelfCgroup)
	if err != nil || group != o.group {
		return fail(errors.New("driver left held unit cgroup"))
	}
	boot, err := cohortReadPath("/proc/sys/kernel/random/boot_id", 128)
	if err != nil || strings.TrimSpace(boot) != o.boot {
		return fail(errors.New("boot epoch changed"))
	}
	pids := map[int]bool{}
	groups := 0
	var walk func(*os.File) error
	walk = func(dir *os.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		groups++
		if groups > 256 {
			return errors.New("cohort cgroup count exceeds bound")
		}
		procs, err := cohortOpenAt(dir, "cgroup.procs", false)
		if err != nil {
			return err
		}
		b, readErr := boundedCohortRead(procs, 65536)
		err = errors.Join(readErr, procs.Close())
		if err != nil {
			return err
		}
		for _, field := range strings.Fields(string(b)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 || pids[pid] {
				return errors.New("invalid/duplicate cohort PID")
			}
			pids[pid] = true
			if len(pids) > 1024 {
				return errors.New("cohort process count exceeds bound")
			}
		}
		// A fresh FD starts directory enumeration at zero for each bookend. Limit
		// enumeration itself, rather than allocating an unbounded entry list first.
		scan, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()))
		if err != nil {
			return err
		}
		entries, readDirErr := scan.ReadDir(4097)
		if readDirErr == io.EOF {
			readDirErr = nil
		}
		if err = errors.Join(readDirErr, scan.Close()); err != nil {
			return err
		}
		if len(entries) > 4096 {
			return errors.New("cgroup directory entries exceed bound")
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			child, err := cohortOpenAt(dir, entry.Name(), true)
			if err != nil {
				return err
			}
			childID, identityErr := cohortUnitIdentity(child)
			if identityErr == nil && (childID.Device != o.identity.Device || childID.MountID != o.identity.MountID) {
				identityErr = errors.New("nested cgroup mount changed")
			}
			if identityErr == nil {
				identityErr = walk(child)
			}
			if err = errors.Join(identityErr, child.Close()); err != nil {
				return err
			}
		}
		return nil
	}
	if err = walk(o.dir); err != nil {
		return fail(err)
	}
	if !pids[out.DriverPID] || !pids[daemonPID] {
		return fail(errors.New("driver/daemon absent from owned unit census"))
	}
	ids := make([]int, 0, len(pids))
	for pid := range pids {
		ids = append(ids, pid)
	}
	sort.Ints(ids)
	for _, pid := range ids {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		prefix := filepath.Join("/proc", strconv.Itoa(pid))
		before, e1 := cohortReadPath(filepath.Join(prefix, "stat"), 4096)
		memberGroup, e2 := cohortReadPath(filepath.Join(prefix, "cgroup"), 4096)
		after, e3 := cohortReadPath(filepath.Join(prefix, "stat"), 4096)
		birth, e4 := projectionBirth(before, pid)
		endBirth, e5 := projectionBirth(after, pid)
		memberPath, e6 := cohortGroup(memberGroup)
		if errors.Join(e1, e2, e3, e4, e5, e6) != nil || birth != endBirth || (memberPath != o.group && !strings.HasPrefix(memberPath, o.group+"/")) {
			return fail(errors.New("member PID birth/cgroup changed or unavailable"))
		}
		out.Members = append(out.Members, cohortProcess{PID: pid, StatBefore: before, StatAfter: after, Cgroup: memberGroup})
	}
	peak, err := cohortOpenAt(o.dir, "memory.peak", false)
	if err != nil {
		return fail(err)
	}
	out.PeakFile, err = cohortIdentity(peak)
	var b []byte
	if err == nil {
		b, err = boundedCohortRead(peak, 64)
	}
	err = errors.Join(err, peak.Close())
	out.RawMemoryPeak = string(b)
	if err != nil {
		return fail(err)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return fail(errors.New("memory.peak is not exact uint64 bytes"))
	}
	// No peak delta or process-peak sum is computed.
	out.PeakBytes = &value
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return out
}
