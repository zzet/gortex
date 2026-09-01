package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// statusOnlyController satisfies just the Status slice of Controller.
// Embedding the nil interface keeps the stub one line: handleControl only
// touches Status for a ControlStatus request, so the unimplemented methods
// are never reached.
type statusOnlyController struct{ Controller }

func (statusOnlyController) Status(context.Context) (StatusResponse, error) {
	return StatusResponse{}, nil
}

// fakeBinaryStat is a swappable binaryStatFn over a real temp file: the test
// flips fail to simulate the status-time stat error path.
type fakeBinaryStat struct {
	fail bool
}

func (f *fakeBinaryStat) stat(path string) (int64, int64, error) {
	if f.fail {
		return 0, 0, errors.New("stat failed")
	}
	return osStatIdentity(path)
}

// newBinaryStaleTestServer builds a Server whose binary identity points at
// a temp file standing in for the daemon executable, captured through the
// fake statFn exactly the way New captures the real one.
func newBinaryStaleTestServer(t *testing.T, logger *zap.Logger) (*Server, *fakeBinaryStat, string) {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "gortex")
	if err := os.WriteFile(binPath, []byte("fake daemon image"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(dir, "sock"), "test", logger)
	fake := &fakeBinaryStat{}
	s.binaryStatFn = fake.stat
	s.captureBinaryIdentity(binPath)
	if s.binaryPath != binPath {
		t.Fatalf("binary identity not captured: binaryPath=%q", s.binaryPath)
	}
	s.Controller = statusOnlyController{}
	return s, fake, binPath
}

// requestStatus drives one ControlStatus request through the real handler
// and decodes the StatusResponse it returns.
func requestStatus(t *testing.T, s *Server) StatusResponse {
	t.Helper()
	resp := s.handleControl(context.Background(), nil, ControlRequest{Kind: ControlStatus})
	if !resp.OK {
		t.Fatalf("control status failed: %s: %s", resp.ErrorCode, resp.ErrorMsg)
	}
	var st StatusResponse
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		t.Fatalf("unmarshal status result: %v", err)
	}
	return st
}

func staleWarningCount(logs *observer.ObservedLogs) int {
	n := 0
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "on-disk binary changed") {
			n++
		}
	}
	return n
}

// TestStatusBinaryDriftFreshThenStale walks the full lifecycle through the
// real ControlStatus handler: fresh at start, stale after an on-disk
// replace (mtime drift), the restart hint logged exactly once, and the
// stale flag stable on the poll that follows.
func TestStatusBinaryDriftFreshThenStale(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	s, _, binPath := newBinaryStaleTestServer(t, zap.New(core))

	// Fresh: the on-disk image still matches the one captured at start.
	st := requestStatus(t, s)
	if !st.BinaryChecked || st.BinaryStale {
		t.Fatalf("fresh status: BinaryChecked=%v BinaryStale=%v, want true/false", st.BinaryChecked, st.BinaryStale)
	}
	if st.BinaryReplacedAtUnix != 0 {
		t.Fatalf("fresh status: BinaryReplacedAtUnix=%d, want 0", st.BinaryReplacedAtUnix)
	}
	if n := staleWarningCount(logs); n != 0 {
		t.Fatalf("fresh status: %d stale warnings, want 0", n)
	}

	// Replace the image under the running daemon: future mtime, same size.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(binPath, future, future); err != nil {
		t.Fatal(err)
	}
	st = requestStatus(t, s)
	if !st.BinaryChecked || !st.BinaryStale {
		t.Fatalf("stale status: BinaryChecked=%v BinaryStale=%v, want true/true", st.BinaryChecked, st.BinaryStale)
	}
	if st.BinaryReplacedAtUnix != future.Unix() {
		t.Fatalf("stale status: BinaryReplacedAtUnix=%d, want %d", st.BinaryReplacedAtUnix, future.Unix())
	}
	if n := staleWarningCount(logs); n != 1 {
		t.Fatalf("first stale detection: %d stale warnings, want 1", n)
	}
	if !s.binaryLoggedStale {
		t.Fatal("first stale detection: binaryLoggedStale once-flag not set")
	}

	// The next stale poll must not log again — a monitoring loop polling
	// status must not spam the daemon log.
	st = requestStatus(t, s)
	if !st.BinaryChecked || !st.BinaryStale {
		t.Fatalf("second stale status: BinaryChecked=%v BinaryStale=%v, want true/true", st.BinaryChecked, st.BinaryStale)
	}
	if n := staleWarningCount(logs); n != 1 {
		t.Fatalf("second stale status: %d stale warnings, want still 1", n)
	}
}

// TestStatusBinaryDriftSizeChange exercises the size half of the identity:
// a grown image with the original mtime restored is still drift.
func TestStatusBinaryDriftSizeChange(t *testing.T) {
	s, _, binPath := newBinaryStaleTestServer(t, nil)
	startMod := s.binaryStartMod

	f, err := os.OpenFile(binPath, os.O_APPEND|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("-v2-now-longer"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Restore the captured mtime so only the size differs.
	if err := os.Chtimes(binPath, time.Unix(startMod, 0), time.Unix(startMod, 0)); err != nil {
		t.Fatal(err)
	}

	var st StatusResponse
	s.populateBinaryStatus(&st)
	if !st.BinaryChecked || !st.BinaryStale {
		t.Fatalf("size drift: BinaryChecked=%v BinaryStale=%v, want true/true", st.BinaryChecked, st.BinaryStale)
	}
}

// TestStatusBinaryStatErrorReportsUnknown: when the status-time stat fails,
// the daemon must report unknown (both flags false), never fresh.
func TestStatusBinaryStatErrorReportsUnknown(t *testing.T) {
	s, fake, _ := newBinaryStaleTestServer(t, nil)
	fake.fail = true

	var st StatusResponse
	s.populateBinaryStatus(&st)
	if st.BinaryChecked || st.BinaryStale {
		t.Fatalf("stat error: BinaryChecked=%v BinaryStale=%v, want false/false (unknown, not fresh)", st.BinaryChecked, st.BinaryStale)
	}
}

// TestStatusBinaryUncapturedIdentityReportsUnknown: a Server whose startup
// capture failed (empty binaryPath) reports unknown on every status.
func TestStatusBinaryUncapturedIdentityReportsUnknown(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "sock"), "test", nil)
	s.binaryPath = "" // simulate the constructor-capture failure path

	var st StatusResponse
	s.populateBinaryStatus(&st)
	if st.BinaryChecked || st.BinaryStale {
		t.Fatalf("uncaptured identity: BinaryChecked=%v BinaryStale=%v, want false/false", st.BinaryChecked, st.BinaryStale)
	}
}
