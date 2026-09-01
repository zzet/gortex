package indexer

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

func TestPickKind_Priority(t *testing.T) {
	cases := []struct {
		name  string
		types []fswatcher.EventType
		want  ChangeKind
	}{
		{"empty", nil, ""},
		{"chmod_only", []fswatcher.EventType{fswatcher.EventChmod}, ""},
		{"create", []fswatcher.EventType{fswatcher.EventCreate}, ChangeCreated},
		{"modify", []fswatcher.EventType{fswatcher.EventMod}, ChangeModified},
		{"remove", []fswatcher.EventType{fswatcher.EventRemove}, ChangeDeleted},
		{"rename", []fswatcher.EventType{fswatcher.EventRename}, ChangeRenamed},
		// FSEvents flag accumulation: a write to an existing file fires
		// with both Create and Modify set. Modify must win, otherwise
		// the modify path's snapshot/eviction logic is skipped.
		{"create_plus_modify", []fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventMod}, ChangeModified},
		// Remove dominates everything else.
		{"all_flags", []fswatcher.EventType{
			fswatcher.EventCreate, fswatcher.EventMod, fswatcher.EventRename, fswatcher.EventRemove,
		}, ChangeDeleted},
		{"rename_plus_create", []fswatcher.EventType{fswatcher.EventCreate, fswatcher.EventRename}, ChangeRenamed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, pickKind(tc.types))
		})
	}
}

func TestNormalizeEventPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		root string
		want string
	}{
		{
			"strips_private_var_when_root_is_var",
			"/private/var/folders/abc/T/repo/main.go",
			"/var/folders/abc/T/repo",
			"/var/folders/abc/T/repo/main.go",
		},
		{
			"strips_private_tmp_when_root_is_tmp",
			"/private/tmp/repo/main.go",
			"/tmp/repo",
			"/tmp/repo/main.go",
		},
		{
			"keeps_private_when_root_also_private",
			"/private/var/x/main.go",
			"/private/var/x",
			"/private/var/x/main.go",
		},
		{
			"keeps_private_when_strip_doesnt_match_root",
			"/private/etc/main.go",
			"/tmp/repo",
			"/private/etc/main.go",
		},
		{
			"unprefixed_path_passes_through",
			"/tmp/repo/main.go",
			"/tmp/repo",
			"/tmp/repo/main.go",
		},
		{
			"empty_root_passes_through",
			"/private/var/x/main.go",
			"",
			"/private/var/x/main.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeEventPath(tc.path, tc.root)
			if runtime.GOOS != "darwin" {
				// Off macOS the function is a no-op.
				assert.Equal(t, tc.path, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
