package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A lossless gate-mode cell keeps, whole, a burst of distinct answers that
// exceeds the default 16 MiB per-edit cap. The same burst under that cap
// still overflows, with lossless or plain capture, and the report says so.
func TestEditProbeLosslessCellKeepsBurstAboveDefaultRawCap(t *testing.T) {
	// Distinct, incompressible answers: neither the repeat fold nor the
	// lossless codecs can shrink this burst below 16 MiB.
	random := rand.NewChaCha8([32]byte{19})
	burst := make([]json.RawMessage, 384)
	for i := range burst {
		raw := make([]byte, 45<<10)
		_, _ = random.Read(raw)
		burst[i] = json.RawMessage(`{"source":"` + base64.StdEncoding.EncodeToString(raw) + `"}`)
	}
	gate := []string{"--binary", "/binary", "--manifest", "/manifest", "--scratch", "/scratch", "--scenario", "edit-and-probe", "--agents", "0", "--edit-agents", "1", "--worktrees", "0", "--edit-gate-modes"}
	for _, cell := range []struct {
		name  string
		flags []string
		limit int
		whole bool
	}{
		{"lossless", []string{"--lossless-edit-probe-evidence"}, 64 << 20, true},
		{"lossless_old_cap", []string{"--lossless-edit-probe-evidence", "--edit-probe-raw-limit-mib", "16"}, 16 << 20, false},
		{"plain", nil, 16 << 20, false},
	} {
		t.Run(cell.name, func(t *testing.T) {
			o, err := parseRun(append(slices.Clone(gate), cell.flags...))
			if err != nil {
				t.Fatal(err)
			}
			run, phase := prepareRawLimitCell(t, o)
			s := run.states[0]
			s.mu.Lock()
			s.record.Outcome = "complete" // the edit itself completed; only its capture is in question
			s.mu.Unlock()
			var emitErr error
			for i, payload := range burst {
				if err := s.emit(map[string]any{"event": "rpc", "purpose": "oracle:source", "wire_id": int64(i + 1), "payload": payload}); err != nil && emitErr == nil {
					emitErr = err
				}
			}
			finishErr := run.finish()
			record := phase.EditProbe.Records[0]
			spool, err := os.ReadFile(record.Evidence)
			if err != nil {
				t.Fatal(err)
			}
			if record.EvidenceLimit != cell.limit || record.EvidenceBytes != len(spool) || record.EvidenceBytes > record.EvidenceLimit {
				t.Fatalf("evidence bytes=%d spool=%d limit=%d, want limit %d", record.EvidenceBytes, len(spool), record.EvidenceLimit, cell.limit)
			}
			incomplete := phase.EditProbe.Summary.Outcomes["harness_evidence_incomplete"]
			overflowed := fmt.Sprintf("edit %d: harness_evidence_incomplete", record.Ordinal)
			if !cell.whole {
				reported := strings.Join(record.Errors, "; ")
				if !errors.Is(emitErr, errEditProbeEvidenceIncomplete) || record.Outcome != "harness_evidence_incomplete" || incomplete != 1 ||
					!strings.Contains(reported, "per-edit raw evidence limit exceeded") || !strings.Contains(reported, fmt.Sprintf("limit_bytes=%d", cell.limit)) ||
					finishErr == nil || !strings.Contains(finishErr.Error(), overflowed) {
					t.Fatalf("overflow not disclosed: emit=%v outcome=%s summary=%d errors=%q", emitErr, record.Outcome, incomplete, reported)
				}
				return
			}
			if emitErr != nil || record.Outcome != "complete" || len(record.Errors) != 0 || incomplete != 0 || finishErr != nil && strings.Contains(finishErr.Error(), overflowed) {
				t.Fatalf("burst not captured whole: emit=%v outcome=%s errors=%q", emitErr, record.Outcome, record.Errors)
			}
			if record.EvidenceBytes <= 16<<20 || record.EvidenceCodec != editProbeLosslessCodec {
				t.Fatalf("burst of %d encoded bytes (%s) does not exercise the default cap", record.EvidenceBytes, record.EvidenceCodec)
			}
			var history editProbePayloadHistory
			var kept []json.RawMessage
			lines := bufio.NewScanner(bytes.NewReader(spool))
			lines.Buffer(nil, 16<<20)
			for lines.Scan() {
				var row map[string]json.RawMessage
				if err := json.Unmarshal(lines.Bytes(), &row); err != nil {
					t.Fatal(err)
				}
				if spoolString(row, "event") != "rpc" {
					continue
				}
				payload, err := history.decode(row)
				if err != nil {
					t.Fatal(err)
				}
				kept = append(kept, payload)
			}
			if err := lines.Err(); err != nil {
				t.Fatal(err)
			}
			if len(kept) != len(burst) {
				t.Fatalf("kept %d of %d answers", len(kept), len(burst))
			}
			for i := range burst {
				if !bytes.Equal(kept[i], burst[i]) {
					t.Fatalf("answer %d changed in the spool", i+1)
				}
			}
		})
	}
}

// The cell is prepared by the production runtime from its parsed options.
func prepareRawLimitCell(t *testing.T, o runOptions) (*editProbeRuntime, *Phase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	c := editProbeCorpus()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, c.File), []byte("package p\nfunc Example() int { return 1 }\n"+c.EditAnchor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := startStub(t, func(stubFrame) (any, *rpcError) { return stubContent(map[string]any{}), nil })
	editor, err := openMCP(ctx, socket, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = editor.Close() })
	phase := &Phase{Metrics: make(map[string]Metric)}
	p := &privateRun{Root: root, Socket: socket, Repos: []privateRepo{{Path: root, Prefix: c.Label, Corpus: c}}}
	run, probers, err := prepareEditProbe(ctx, o, p, nil, []*socketClient{editor}, phase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = run.close()
		for _, client := range probers {
			_ = client.Close()
		}
	})
	return run, phase
}
