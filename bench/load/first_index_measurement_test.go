package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestMeasureStandaloneFirstIndex is an opt-in, single-build harness. Run the
// identical harness on the control and candidate with an owned clean corpus;
// compare reports separately. It never grants a ratio from a single run.
func TestMeasureStandaloneFirstIndex(t *testing.T) {
	corpus := os.Getenv("GORTEX_FIRST_INDEX_CORPUS")
	if corpus == "" {
		t.Skip("set GORTEX_FIRST_INDEX_CORPUS and GORTEX_FIRST_INDEX_REPORT for an owned-fixture measurement")
	}
	reportPath := os.Getenv("GORTEX_FIRST_INDEX_REPORT")
	if reportPath == "" {
		t.Fatal("GORTEX_FIRST_INDEX_REPORT is required")
	}
	corpus, err := filepath.Abs(corpus)
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		result, err := exec.Command("git", append([]string{"-C", corpus}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %v: %v: %s", args, err, result)
		}
		return string(result)
	}
	if status := git("status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("fixture must be clean: %s", status)
	}
	sourceSHA := strings.TrimSpace(git("rev-parse", "HEAD"))
	manifest := sha256.Sum256([]byte(git("ls-files", "-s", "-z")))
	output, err := os.OpenFile(reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	sampler := newProcessSampler()
	type sample struct {
		At      time.Time         `json:"at"`
		Metrics map[string]Metric `json:"metrics"`
	}
	var mu sync.Mutex
	var samples []sample
	var before, after map[string]Metric
	var startAt, endAt time.Time
	started, finished, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var startOnce, endOnce sync.Once
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		switch entry.Message {
		case "indexer: drain start (shadow → disk)":
			startOnce.Do(func() {
				mu.Lock()
				startAt = entry.Time
				before = sampler.sample(os.Getpid())
				samples = append(samples, sample{time.Now(), before})
				mu.Unlock()
				close(started)
			})
		case "indexer: FlushBulk complete":
			endOnce.Do(func() {
				mu.Lock()
				endAt = entry.Time
				after = sampler.sample(os.Getpid())
				samples = append(samples, sample{time.Now(), after})
				mu.Unlock()
				close(finished)
			})
		}
		return nil
	}))
	go func() {
		defer close(joined)
		select {
		case <-started:
		case <-finished:
			return
		}
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-finished:
				return
			case <-ticker.C:
				mu.Lock()
				if endAt.IsZero() {
					samples = append(samples, sample{time.Now(), sampler.sample(os.Getpid())})
				}
				mu.Unlock()
			}
		}
	}()
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	idx := indexer.New(store, registry, config.Default().Index, logger)
	t.Cleanup(idx.Close)
	result, indexErr := idx.IndexCtx(t.Context(), corpus)
	endOnce.Do(func() { close(finished) })
	<-joined
	complete := indexErr == nil && !startAt.IsZero() && !endAt.IsZero()
	deltas := make(map[string]Metric)
	for _, name := range []string{"diskio_written_bytes", "logical_written_bytes", "syscall_written_bytes"} {
		if complete {
			deltas[name] = counterDelta(before[name], after[name])
		} else {
			deltas[name] = unavailable("bytes", "complete drain endpoints unavailable")
		}
	}
	entries := logs.FilterMessage("indexer: FlushBulk complete").All()
	var drainFields map[string]any
	if len(entries) == 1 {
		drainFields = entries[0].ContextMap()
	}
	report := map[string]any{
		"go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0),
		"complete": complete, "corpus_sha": sourceSHA, "tracked_manifest_sha256": hex.EncodeToString(manifest[:]),
		"result": result, "drain_start": startAt, "drain_end": endAt, "drain_fields": drainFields,
		"process_write_deltas": deltas, "process_samples": samples,
		"qualification": "process-wide counters over drain log boundaries; concurrent process activity can contribute; macOS logical writes exclude external-volume ledger; diskio is the physical meter; unavailable observations remain null; five-second samples plus endpoints; compare identical frozen corpus/harness/host/storage separately; no acceptance inferred",
	}
	if complete {
		report["drain_and_seal_seconds"] = endAt.Sub(startAt).Seconds()
	}
	if indexErr != nil {
		report["error"] = indexErr.Error()
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		t.Fatal(err)
	}
	if status := git("status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("fixture changed: %s", status)
	}
	if strings.TrimSpace(git("rev-parse", "HEAD")) != sourceSHA || sha256.Sum256([]byte(git("ls-files", "-s", "-z"))) != manifest {
		t.Fatal("fixture identity changed")
	}
	if !complete {
		t.Fatalf("first index did not complete one measured drain: %v", indexErr)
	}
}
