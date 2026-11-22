package indexer

import (
	"os"
	"testing"
)

func TestCgroupMemoryLimitFromNestedV2(t *testing.T) {
	const gib = uint64(1) << 30
	tests := []struct {
		name  string
		files map[string]string
		want  uint64
	}{
		{
			name: "finite leaf below unlimited system slice",
			files: map[string]string{
				"/proc/self/cgroup":    "0::/system.slice/owned-load.service\n",
				"/proc/self/mountinfo": "48 32 0:29 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n",
				"/sys/fs/cgroup/system.slice/owned-load.service/memory.max": "17179869184\n",
				"/sys/fs/cgroup/system.slice/memory.max":                    "max\n",
				"/sys/fs/cgroup/memory.max":                                 "max\n",
			},
			want: 16 * gib,
		},
		{
			name: "tightest visible finite ancestor including mount root",
			files: map[string]string{
				"/proc/self/cgroup":    "0::/system.slice/owned-load.service\n",
				"/proc/self/mountinfo": "48 32 0:29 / /sys/fs/cgroup rw shared:7 - cgroup2 cgroup rw\n",
				"/sys/fs/cgroup/system.slice/owned-load.service/memory.max": "17179869184\n",
				"/sys/fs/cgroup/system.slice/memory.max":                    "12884901888\n",
				"/sys/fs/cgroup/memory.max":                                 "15032385536\n",
			},
			want: 12 * gib,
		},
		{
			name: "delegated mount escapes and component prefix",
			files: map[string]string{
				"/proc/self/cgroup": "8:cpu:/legacy\n0::/tenant old/worker.service\n",
				"/proc/self/mountinfo": "48 32 0:29 /tenant /wrong rw - cgroup2 cgroup rw\n" +
					"49 32 0:29 /tenant\\040old /run/cgroup\\040v2 rw - cgroup2 cgroup rw\n" +
					"50 32 0:22 / /sys/other rw - tmpfs tmpfs rw\n",
				"/run/cgroup v2/worker.service/memory.max":    "max\n",
				"/run/cgroup v2/memory.max":                   "6442450944\n",
				"/sys/fs/cgroup/memory.max":                   "max\n",
				"/sys/fs/cgroup/memory/memory.limit_in_bytes": "max\n",
			},
			want: 6 * gib,
		},
		{
			name: "namespace relative membership starts at mounted root",
			files: map[string]string{
				"/proc/self/cgroup":                           "0::/\n",
				"/proc/self/mountinfo":                        "49 32 0:29 / /run/unified rw - cgroup2 cgroup rw\n",
				"/run/unified/memory.max":                     "8589934592\n",
				"/sys/fs/cgroup/memory.max":                   "max\n",
				"/sys/fs/cgroup/memory/memory.limit_in_bytes": "max\n",
			},
			want: 8 * gib,
		},
		{
			name: "reject paths containing parent traversal",
			files: map[string]string{
				"/proc/self/cgroup":                           "0::/system.slice/../../outside\n",
				"/sys/fs/cgroup/memory.max":                   "max\n",
				"/sys/fs/cgroup/memory/memory.limit_in_bytes": "4294967296\n",
			},
			want: 4 * gib,
		},
		{
			name: "reject unconfined mountpoint and unrelated v2 mount",
			files: map[string]string{
				"/proc/self/cgroup": "0::/tenant-old/job\n",
				"/proc/self/mountinfo": "49 32 0:29 / /run/groups/../arbitrary rw - cgroup2 cgroup rw\n" +
					"50 32 0:29 /tenant /separate rw - cgroup2 cgroup rw\n",
				"/sys/fs/cgroup/memory.max":                   "max\n",
				"/sys/fs/cgroup/memory/memory.limit_in_bytes": "4294967296\n",
			},
			want: 4 * gib,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readFile := func(file string) ([]byte, error) {
				if data, found := tt.files[file]; found {
					return []byte(data), nil
				}
				if file != "/sys/fs/cgroup/memory/memory.limit_in_bytes" {
					t.Fatalf("read outside fixture process ancestry: %q", file)
				}
				return nil, os.ErrNotExist
			}
			if got := cgroupMemoryLimitFrom(readFile); got != tt.want {
				t.Fatalf("effective cgroup memory limit = %d, want %d", got, tt.want)
			} else if got := indexMemoryBudget(20918583296, got); got != int64(tt.want/2) {
				t.Fatalf("cold soft budget = %d, want %d", got, tt.want/2)
			}
		})
	}
}

func TestCgroupMemoryLimitFromMountInfoFraming(t *testing.T) {
	const (
		group = "/system.slice/owned-load.service"
		limit = uint64(16) << 30
	)
	tests := []struct {
		name   string
		mounts string
		real   string
		decoys []string
	}{
		{
			name:   "literal nonbreaking space stays in the mountpoint",
			mounts: "48 32 0:29 / /run/cgroup\u00a0v2 rw - cgroup2 cgroup rw\n",
			real:   "/run/cgroup\u00a0v2",
			decoys: []string{"/run/cgroup"},
		},
		{
			name: "truncated records cannot select a mount",
			mounts: "48 32 0:29 / /run/no-source rw - cgroup2 \n" +
				"49 32 0:29 / /run/no-super-options rw - cgroup2 cgroup\n" +
				"50 32 0:29 / /run/valid rw - cgroup2 cgroup rw\n",
			real:   "/run/valid",
			decoys: []string{"/run/no-source", "/run/no-super-options"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var decoyReads []string
			readFile := func(file string) ([]byte, error) {
				switch file {
				case "/proc/self/cgroup":
					return []byte("0::" + group + "\n"), nil
				case "/proc/self/mountinfo":
					return []byte(tt.mounts), nil
				case tt.real + group + "/memory.max":
					return []byte("17179869184\n"), nil
				case tt.real + "/system.slice/memory.max", tt.real + "/memory.max", "/sys/fs/cgroup/memory.max":
					return []byte("max\n"), nil
				}
				for _, decoy := range tt.decoys {
					if file == decoy+group+"/memory.max" || file == decoy+"/system.slice/memory.max" || file == decoy+"/memory.max" {
						decoyReads = append(decoyReads, file)
						return []byte("2147483648\n"), nil
					}
				}
				return nil, os.ErrNotExist
			}
			if got := cgroupMemoryLimitFrom(readFile); got != limit || len(decoyReads) != 0 {
				t.Fatalf("effective cgroup limit = %d, want %d; decoy reads: %q", got, limit, decoyReads)
			}
		})
	}
}
