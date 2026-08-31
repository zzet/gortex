package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func useAmpleBulkPressureFixture(s *Store) {
	s.bulkWALPressureProbe = func() bulkWALPressureSnapshot {
		return bulkWALPressureSnapshot{DiskAvailableKnown: true, DiskAvailableBytes: 64 << 30}
	}
}

func TestBulkWALPressureLimitsAreBounded(t *testing.T) {
	const gib = int64(1 << 30)
	tests := []struct {
		name                string
		dbBytes             int64
		soft, hard, reserve int64
	}{
		{name: "empty store", dbBytes: 0, soft: 2 * gib, hard: 4 * gib, reserve: gib},
		{name: "medium store", dbBytes: 6 * gib, soft: 3 * gib, hard: 6 * gib, reserve: gib + gib/2},
		{name: "large store", dbBytes: 64 * gib, soft: 4 * gib, hard: 8 * gib, reserve: 8 * gib},
		{name: "negative telemetry", dbBytes: -1, soft: 2 * gib, hard: 4 * gib, reserve: gib},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			soft, hard, reserve := bulkWALPressureLimits(test.dbBytes)
			if soft != test.soft || hard != test.hard || reserve != uint64(test.reserve) {
				t.Fatalf("limits = (%d, %d, %d), want (%d, %d, %d)",
					soft, hard, reserve, test.soft, test.hard, test.reserve)
			}
		})
	}
}

func TestBulkWALPressureRequiresAnIncompleteCheckpoint(t *testing.T) {
	const gib = int64(1 << 30)
	result := walCheckpointResult{Busy: 1, WALFrames: 2_000_000, CheckpointedFrames: 1_000_000}
	incomplete := errors.New("reader-limited checkpoint")

	ample := bulkWALPressureSnapshot{
		DBBytes: 4 * gib, WALBytes: 4 * gib,
		DiskAvailableKnown: true, DiskAvailableBytes: uint64(64 * gib),
	}
	if !ample.needsCheckpoint() {
		t.Fatal("hard WAL limit did not request a checkpoint")
	}
	if err := ample.failureAfterCheckpoint(result, nil); err != nil {
		t.Fatalf("complete checkpoint rejected reusable WAL: %v", err)
	}
	reusable := ample
	reusable.WALBytes = 8 * gib
	reusable.ReusableWALBytes = 8 * gib
	if reusable.needsCheckpoint() {
		t.Fatal("fully reusable physical WAL high-water mark caused a checkpoint storm")
	}
	if err := reusable.failureAfterCheckpoint(result, incomplete); err != nil {
		t.Fatalf("reusable high-water mark counted as active growth: %v", err)
	}
	err := ample.failureAfterCheckpoint(result, incomplete)
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("incomplete hard-limit checkpoint = %v, want ErrBulkLoadWALPressure", err)
	}

	lowDisk := ample
	lowDisk.WALBytes = 1
	lowDisk.DiskAvailableBytes = uint64(gib)
	if err := lowDisk.failureAfterCheckpoint(walCheckpointResult{}, nil); !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("reserve exhaustion = %v, want ErrBulkLoadWALPressure", err)
	}

	nearHeadroom := ample
	nearHeadroom.WALBytes = gib / 2
	nearHeadroom.DiskAvailableBytes = uint64(gib + (32 << 20))
	if err := nearHeadroom.failureAfterCheckpoint(result, incomplete); !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("reader-limited low-headroom WAL = %v, want ErrBulkLoadWALPressure", err)
	}
}

func TestBulkWALCheckpointPreflightDoesNotChargeReusableHighWater(t *testing.T) {
	const gib = int64(1 << 30)
	snapshot := bulkWALPressureSnapshot{
		DBBytes: 4 * gib, WALBytes: 4 * gib, ReusableWALBytes: 4 * gib,
		DiskAvailableKnown: true, DiskAvailableBytes: uint64(5 * gib),
	}
	if snapshot.walGrowthBytes() != 0 {
		t.Fatal("fixture must have no physical growth beyond reusable WAL")
	}
	if err := snapshot.failureBeforeCheckpoint(); err != nil {
		t.Fatalf("reusable high-water preflight = %v, want reusable allocation ignored", err)
	}
}

func TestBulkWALInspectionAdvancesAndTracksPhysicalShrink(t *testing.T) {
	const gib = int64(1 << 30)
	s := &Store{storeCore: &storeCore{
		bulkReusableWALBytes:   8 * gib,
		bulkWALInspectionBytes: 4 * gib,
	}}
	if growth := s.bulkWALGrowthSinceReusable(gib); growth != gib || s.bulkReusableWALBytes != 0 {
		t.Fatalf("shrunken/regrown WAL = growth %d baseline %d, want %d/0", growth, s.bulkReusableWALBytes, gib)
	}
	if s.bulkWALInspectionBytes != bulkWALInspectionMin {
		t.Fatalf("shrunken WAL inspection threshold = %d, want %d", s.bulkWALInspectionBytes, bulkWALInspectionMin)
	}
	if growth := s.bulkWALGrowthSinceReusable(3 * gib); growth != 3*gib {
		t.Fatalf("regrown WAL growth = %d, want %d", growth, 3*gib)
	}

	snapshot := bulkWALPressureSnapshot{
		DBBytes: 4 * gib, WALBytes: 2 * gib,
		DiskAvailableKnown: true, DiskAvailableBytes: uint64(64 * gib),
	}
	next := snapshot.nextInspectionBytes()
	if next <= snapshot.walGrowthBytes() || next > snapshot.walGrowthBytes()+bulkWALInspectionStep {
		t.Fatalf("next inspection = %d for growth %d", next, snapshot.walGrowthBytes())
	}
	lowHeadroom := snapshot
	lowHeadroom.WALBytes = 128 << 20
	lowHeadroom.DiskAvailableBytes = uint64(gib + (256 << 20))
	step := lowHeadroom.nextInspectionBytes() - lowHeadroom.walGrowthBytes()
	if step != bulkWALInspectionMin {
		t.Fatalf("low-headroom inspection step = %d, want %d", step, bulkWALInspectionMin)
	}
}

func TestBulkWALPressureFencesQueuedWritesAndAbortsFinalization(t *testing.T) {
	s, _ := openTempStore(t)
	const gib = int64(1 << 30)
	var walBytes int64
	s.bulkWALPressureProbe = func() bulkWALPressureSnapshot {
		return bulkWALPressureSnapshot{
			DBBytes: 4 * gib, WALBytes: walBytes,
			DiskAvailableKnown: true, DiskAvailableBytes: uint64(64 * gib),
		}
	}
	s.bulkWALSizeProbe = func() (int64, error) { return walBytes, nil }
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated fast path did not engage")
	}
	walBytes = 4 * gib
	// One huge-byte/low-row batch must hit the byte guard before any row cadence
	// or prior checkpoint backoff exists.
	attempts := 0
	s.bulkPassiveCheckpoint = func(context.Context, *sql.Conn, string) (walCheckpointResult, error) {
		attempts++
		result := walCheckpointResult{Busy: 1, WALFrames: 1_500_000, CheckpointedFrames: 750_000}
		return result, errors.Join(errSQLiteCheckpointIncomplete, errors.New("reader-limited checkpoint"))
	}
	first := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "repo/a.go"}
	err := s.AddBatchChecked([]*graph.Node{first}, nil)
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("first pressure error = %v, want ErrBulkLoadWALPressure", err)
	}
	var storageErr *StorageError
	if !errors.As(err, &storageErr) {
		t.Fatalf("first pressure error type = %T, want *StorageError", err)
	}
	var pressureErr *BulkLoadWALPressureError
	if !errors.As(err, &pressureErr) || !pressureErr.Committed() {
		t.Fatalf("pressure error = %#v, want batch_committed=true", pressureErr)
	}
	if got := SafeStorageFailureReason(err); got != "graph storage stopped before disk exhaustion; release long-running graph readers or free disk space, then retry" {
		t.Fatalf("safe pressure reason = %q", got)
	}
	if s.GetNode(first.ID) == nil {
		t.Fatal("pressure detection must report that the triggering batch already committed")
	}

	second := &graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "repo/b.go"}
	err = s.AddBatchChecked([]*graph.Node{second}, nil)
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("queued write pressure error = %v, want sticky ErrBulkLoadWALPressure", err)
	}
	if s.GetNode(second.ID) != nil {
		t.Fatal("sticky pressure fence allowed a queued batch to commit")
	}
	if attempts != 1 {
		t.Fatalf("checkpoint attempts = %d, want exactly one", attempts)
	}

	err = s.EndCoordinatedBulkLoad()
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("terminal finalization = %v, want pressure failure", err)
	}
	if s.bulkConn != nil || s.coordinatedBulkLoad {
		t.Fatal("terminal finalization retained the unsafe bulk writer")
	}
	third := &graph.Node{ID: "repo/c.go::C", Kind: graph.KindFunction, Name: "C", FilePath: "repo/c.go"}
	err = s.AddBatchChecked([]*graph.Node{third}, nil)
	if !errors.Is(err, ErrBulkLoadWALPressure) || s.GetNode(third.ID) != nil {
		t.Fatalf("post-finalize queued write = (%v, node=%v), want fenced", err, s.GetNode(third.ID))
	}
	if err := s.AbortCoordinatedBulkLoad(); err != nil {
		t.Fatalf("retire terminal fence: %v", err)
	}
}

func TestBulkWALInitialHeadroomRefusesFirstWriteBeforeCommit(t *testing.T) {
	s, _ := openTempStore(t)
	const gib = int64(1 << 30)
	s.bulkWALPressureProbe = func() bulkWALPressureSnapshot {
		return bulkWALPressureSnapshot{
			DBBytes: 0, WALBytes: 0,
			DiskAvailableKnown: true, DiskAvailableBytes: uint64(gib + (32 << 20)),
		}
	}
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated fast path did not engage")
	}
	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"}
	err := s.AddBatchChecked([]*graph.Node{node}, nil)
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("initial headroom error = %v, want ErrBulkLoadWALPressure", err)
	}
	var pressureErr *BulkLoadWALPressureError
	if !errors.As(err, &pressureErr) || pressureErr.Committed() {
		t.Fatalf("initial pressure error = %#v, want committed=false", pressureErr)
	}
	if s.GetNode(node.ID) != nil {
		t.Fatal("initial capacity fence allowed first transaction to commit")
	}
	if err := s.AbortCoordinatedBulkLoad(); err != nil {
		t.Fatalf("AbortCoordinatedBulkLoad: %v", err)
	}
}

func TestBulkWALProbeFailureFailsClosedWithoutCommit(t *testing.T) {
	s, _ := openTempStore(t)
	s.bulkLastWALBytes = 123
	s.bulkWALSizeProbe = func() (int64, error) { return 0, os.ErrPermission }
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated fast path did not engage")
	}
	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"}
	err := s.AddBatchChecked([]*graph.Node{node}, nil)
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("probe failure = %v, want ErrBulkLoadWALPressure", err)
	}
	var pressureErr *BulkLoadWALPressureError
	if !errors.As(err, &pressureErr) || pressureErr.Committed() || !pressureErr.WALProbeFailed {
		t.Fatalf("probe failure contract = %#v, want failed probe and committed=false", pressureErr)
	}
	if s.GetNode(node.ID) != nil {
		t.Fatal("unknown WAL size allowed first write")
	}
	if err := s.AbortCoordinatedBulkLoad(); err != nil {
		t.Fatalf("AbortCoordinatedBulkLoad: %v", err)
	}
}

func TestBulkWALProbeFailureAfterCommitFencesGeneration(t *testing.T) {
	s, _ := openTempStore(t)
	probeErr := false
	s.bulkWALSizeProbe = func() (int64, error) {
		if probeErr {
			return 0, os.ErrPermission
		}
		return 0, nil
	}
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated fast path did not engage")
	}
	probeErr = true
	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"}
	err := s.AddBatchChecked([]*graph.Node{node}, nil)
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("mid-load probe failure = %v, want ErrBulkLoadWALPressure", err)
	}
	var pressureErr *BulkLoadWALPressureError
	if !errors.As(err, &pressureErr) || !pressureErr.Committed() || !pressureErr.WALProbeFailed {
		t.Fatalf("mid-load probe contract = %#v, want failed probe and committed=true", pressureErr)
	}
	if s.GetNode(node.ID) == nil {
		t.Fatal("mid-load probe failure did not expose the committed triggering batch")
	}
	queued := &graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B"}
	if err := s.AddBatchChecked([]*graph.Node{queued}, nil); !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("queued write after probe failure = %v", err)
	}
	if s.GetNode(queued.ID) != nil {
		t.Fatal("probe failure fence allowed queued generation write")
	}
	if err := s.AbortCoordinatedBulkLoad(); err != nil {
		t.Fatalf("AbortCoordinatedBulkLoad: %v", err)
	}
}

func TestPostCommitCheckpointFailureExposesCommittedContract(t *testing.T) {
	s, _ := openTempStore(t)
	useAmpleBulkPressureFixture(s)
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated fast path did not engage")
	}
	want := errors.New("checkpoint disk I/O failure")
	s.bulkPassiveCheckpoint = func(context.Context, *sql.Conn, string) (walCheckpointResult, error) {
		return walCheckpointResult{}, want
	}
	s.bulkCheckpointNodeRows = bulkCheckpointNodeInterval - 1
	s.bulkPressureNodeRows = bulkCheckpointNodeInterval - 1
	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"}
	err := s.AddBatchChecked([]*graph.Node{node}, nil)
	if !errors.Is(err, want) {
		t.Fatalf("checkpoint failure = %v, want wrapped %v", err, want)
	}
	var committed *CommittedStorageError
	if !errors.As(err, &committed) || !committed.Committed() {
		t.Fatalf("checkpoint failure type = %T %v, want committed marker", err, err)
	}
	if s.GetNode(node.ID) == nil {
		t.Fatal("post-commit checkpoint error lost committed row")
	}
	if err := s.AbortCoordinatedBulkLoad(); err != nil {
		t.Fatalf("AbortCoordinatedBulkLoad: %v", err)
	}
}

func TestIncompletePressureCheckpointAdvancesByteWatermark(t *testing.T) {
	s, _ := openTempStore(t)
	const gib = int64(1 << 30)
	var walBytes int64
	s.bulkWALSizeProbe = func() (int64, error) { return walBytes, nil }
	fullProbes := 0
	s.bulkWALPressureProbe = func() bulkWALPressureSnapshot {
		fullProbes++
		return bulkWALPressureSnapshot{
			DBBytes: 4 * gib, WALBytes: walBytes,
			DiskAvailableKnown: true, DiskAvailableBytes: uint64(64 * gib),
		}
	}
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated fast path did not engage")
	}
	// Exclude the intentional initial capacity sample from steady-state counts.
	fullProbes = 0
	walBytes = 2 * gib
	attempts := 0
	s.bulkPassiveCheckpoint = func(context.Context, *sql.Conn, string) (walCheckpointResult, error) {
		attempts++
		result := walCheckpointResult{Busy: 1, WALFrames: 1_000_000, CheckpointedFrames: 500_000}
		return result, fmt.Errorf("%w: reader pinned", errSQLiteCheckpointIncomplete)
	}
	first := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"}
	if err := s.AddBatchChecked([]*graph.Node{first}, nil); err != nil {
		t.Fatalf("nonterminal pressure checkpoint: %v", err)
	}
	if s.bulkWALInspectionBytes <= 2*gib {
		t.Fatalf("inspection watermark = %d, want beyond current growth", s.bulkWALInspectionBytes)
	}
	second := &graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B"}
	if err := s.AddBatchChecked([]*graph.Node{second}, nil); err != nil {
		t.Fatalf("below advanced watermark: %v", err)
	}
	if attempts != 1 || fullProbes != 2 {
		t.Fatalf("checkpoint/probe storm: attempts=%d full_probes=%d, want 1/2", attempts, fullProbes)
	}
	if err := s.AbortCoordinatedBulkLoad(); err != nil {
		t.Fatalf("AbortCoordinatedBulkLoad: %v", err)
	}
}

func TestBulkCheckpointBackoffCapsAndResets(t *testing.T) {
	s := &Store{storeCore: &storeCore{bulkCheckpointBackoffShift: bulkCheckpointMaxBackoffShift}}
	if err := s.noteBulkRowCheckpointResultLocked(context.DeadlineExceeded); err != nil {
		t.Fatalf("bounded retry classification: %v", err)
	}
	if s.bulkCheckpointBackoffShift != bulkCheckpointMaxBackoffShift {
		t.Fatalf("backoff shift = %d, want capped %d", s.bulkCheckpointBackoffShift, bulkCheckpointMaxBackoffShift)
	}
	nodes, edges := s.bulkCheckpointIntervalsLocked()
	if nodes != bulkCheckpointNodeInterval<<bulkCheckpointMaxBackoffShift ||
		edges != bulkCheckpointEdgeInterval<<bulkCheckpointMaxBackoffShift {
		t.Fatalf("capped intervals = (%d, %d)", nodes, edges)
	}
	if err := s.noteBulkRowCheckpointResultLocked(nil); err != nil || s.bulkCheckpointBackoffShift != 0 {
		t.Fatalf("successful checkpoint did not restore base cadence: err=%v shift=%d", err, s.bulkCheckpointBackoffShift)
	}
	want := errors.New("disk I/O failure")
	if got := s.noteBulkRowCheckpointResultLocked(want); !errors.Is(got, want) {
		t.Fatalf("non-transient checkpoint failure = %v, want %v", got, want)
	}
}

func TestTerminalBulkPressureRestoresWriterWithoutIndexSeal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terminal.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	useAmpleBulkPressureFixture(s)
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated fast path did not engage")
	}
	s.bulkTerminalErr = &BulkLoadWALPressureError{Reason: "test"}
	var indexBuilds int
	s.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
		if event.Stage == "index" || event.Stage == "index_seal" {
			indexBuilds++
		}
	}
	err = s.Close()
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("Close = %v, want pressure error", err)
	}
	if indexBuilds != 0 {
		t.Fatalf("terminal Close attempted %d index-build stages", indexBuilds)
	}
	if s.bulkConn != nil || s.coordinatedBulkLoad {
		t.Fatal("terminal Close retained bulk state")
	}
}

func TestTerminalOrdinaryBulkFlushRestoresWriter(t *testing.T) {
	s, _ := openTempStore(t)
	useAmpleBulkPressureFixture(s)
	s.BeginBulkLoad()
	if s.bulkConn == nil {
		t.Fatal("ordinary bulk fast path did not engage")
	}
	s.bulkTerminalErr = &BulkLoadWALPressureError{Reason: "test"}
	err := s.FlushBulk()
	if !errors.Is(err, ErrBulkLoadWALPressure) {
		t.Fatalf("FlushBulk = %v, want pressure error", err)
	}
	if s.bulkConn != nil || s.bulkTerminalErr == nil {
		t.Fatal("terminal FlushBulk did not restore writer while retaining the queued-write fence")
	}
	if err := s.AbortCoordinatedBulkLoad(); err != nil {
		t.Fatalf("retire terminal fence: %v", err)
	}
	if s.bulkTerminalErr != nil {
		t.Fatal("explicit abort retained terminal fence")
	}
}

func BenchmarkBulkCheckpointHealthyCadence(b *testing.B) {
	s := &Store{storeCore: &storeCore{bulkConn: &sql.Conn{}}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		// Keep the benchmark on the healthy pre-cadence path; checkpoint and
		// filesystem costs have their own explicit boundaries.
		if s.bulkCheckpointNodeRows >= bulkCheckpointNodeInterval-1 {
			s.bulkCheckpointNodeRows = 0
			s.bulkPressureNodeRows = 0
		}
		if err := s.noteBulkRowsLocked(1, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBulkWALPressurePolicy(b *testing.B) {
	const gib = int64(1 << 30)
	snapshot := bulkWALPressureSnapshot{
		DBBytes: 6 * gib, WALBytes: 3 * gib,
		DiskAvailableKnown: true, DiskAvailableBytes: uint64(32 * gib),
	}
	b.ReportAllocs()
	for range b.N {
		if !snapshot.needsCheckpoint() {
			b.Fatal("pressure fixture did not request a checkpoint")
		}
	}
}

func BenchmarkBulkWALSizeProbe(b *testing.B) {
	path := filepath.Join(b.TempDir(), "graph.sqlite")
	if err := os.WriteFile(path+"-wal", make([]byte, 4096), 0o600); err != nil {
		b.Fatal(err)
	}
	s := &Store{storeCore: &storeCore{dbPath: path}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := s.currentBulkWALBytes(); got != 4096 {
			b.Fatalf("WAL bytes = %d, want 4096", got)
		}
	}
}
