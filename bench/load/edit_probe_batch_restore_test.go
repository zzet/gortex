package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// These are restore ownership controls, not native acceptance or timing proof.
func TestOneFileBatchRestorePreservesOwnedAfterImage(t *testing.T) {
	for _, tc := range []struct {
		name, current    string
		terminal, refuse bool
	}{
		{"owned", "owned-after", true, false},
		{"foreign-after-image", "foreign-after", true, true},
		{"pending", "owned-after", false, true},
		{"foreign-path", "owned-after", true, true},
		{"missing-receipt", "owned-after", true, true},
		{"foreign-relative-path", "owned-after", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file.go")
			if err := os.WriteFile(path, []byte(tc.current), 0600); err != nil {
				t.Fatal(err)
			}
			refs, err := decodeEditProbeBatchAcknowledgement(editProbeBatchACKFixture(), "transaction-1", editProbeBatchACKAbsPath(), "repo/file.go", editProbeBatchOfferedBytes())
			if err != nil {
				t.Fatal(err)
			}
			refs.Path, refs.RelativePath = path, "fixture/file.go"
			refs.AfterSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("owned-after")))
			switch tc.name {
			case "foreign-path":
				refs.Path = filepath.Join(root, "foreign.go")
			case "foreign-relative-path":
				refs.RelativePath = "foreign/file.go"
			case "missing-receipt":
				refs.Observation = ""
			}
			var writes atomic.Int64
			socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
				if frame.Params["name"] == "edit" {
					writes.Add(1)
					if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
						t.Error(err)
					}
				}
				return stubContent(map[string]any{"status": "committed"}), nil
			})
			client, err := openMCP(t.Context(), socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			run := &editProbeRuntime{p: &privateRun{Repos: []privateRepo{{Path: root, Prefix: "fixture"}}}, editors: []*socketClient{client}, plan: editProbePlan{Tool: "edit.batch", File: "file.go"}, original: []byte("before"), options: runOptions{Deadline: time.Second}, phase: &Phase{EditProbe: &editProbeReport{Records: []editProbeRecord{{Batch: &refs, GraphTerminal: tc.terminal}}}}}
			err = run.restore(t.Context())
			if tc.refuse {
				if err == nil || writes.Load() != 0 {
					t.Fatalf("unsafe restore: err=%v writes=%d", err, writes.Load())
				}
				raw, readErr := os.ReadFile(path)
				if readErr != nil || string(raw) != tc.current {
					t.Fatalf("refusal changed bytes: %q %v", raw, readErr)
				}
			} else if err != nil || writes.Load() != 1 {
				t.Fatalf("owned terminal restore: err=%v writes=%d", err, writes.Load())
			}
		})
	}
}
