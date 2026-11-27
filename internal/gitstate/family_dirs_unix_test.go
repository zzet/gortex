//go:build unix

package gitstate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMarkerCommonDirRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	for _, marker := range []string{".git", "commondir"} {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".git")
			if marker == "commondir" {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, marker)
			}
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var gotRoot, common string
			var ok bool
			go func() { gotRoot, common, ok = MarkerCommonDir(root); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				// Release a regressed blocking open before fixture cleanup.
				fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
				if err == nil {
					_ = unix.Close(fd)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("marker worker did not recover")
				}
				t.Fatal("marker read waited for a FIFO writer")
			}
			if ok || common != "" || gotRoot != root {
				t.Fatalf("FIFO lost ambiguous marker evidence: root=%q common=%q ok=%v", gotRoot, common, ok)
			}
		})
	}
}
