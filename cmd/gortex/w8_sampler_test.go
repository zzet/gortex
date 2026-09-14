package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The instrument the W8 sustained-workload harness measures with: a 1 Hz
// process/file sampler, a WAL-header checkpoint counter, a run manifest, and a
// destination census that attributes every byte under the private daemon root
// to a named writer.
//
// Everything in this file is pure enough to unit-test without a daemon, which
// is the point: an instrument nobody has calibrated produces numbers nobody
// should believe.

// ------------------------------------------------------------ WAL header ---

// w8WALHeader is the 32-byte SQLite write-ahead-log header. All fields are
// big-endian (file-format spec: magic@0, format@4, page size@8,
// checkpoint sequence@12, salt-1@16, salt-2@20, checksums@24/@28).
//
// Whether modernc.org/sqlite actually advances CheckpointSeq on a WAL reset is
// answered experimentally by TestW8WALCheckpointSequenceAdvancesOnReset in this
// file rather than assumed from the specification.
type w8WALHeader struct {
	Present       bool
	Magic         uint32
	Format        uint32
	PageSize      uint32
	CheckpointSeq uint32
	Salt1, Salt2  uint32
	Bytes         int64
}

const (
	w8WALMagicBE = 0x377f0683
	w8WALMagicLE = 0x377f0682
)

// w8ReadWALHeader reads the header of a -wal file. An absent or truncated WAL
// is a legitimate state (a TRUNCATE checkpoint leaves a zero-length file), so it
// returns Present=false and no error; only an unreadable file or a foreign
// magic is an error.
func w8ReadWALHeader(path string) (w8WALHeader, error) {
	var header w8WALHeader
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return header, nil
		}
		return header, err
	}
	header.Bytes = info.Size()
	if info.Size() < 32 {
		return header, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return header, err
	}
	defer func() { _ = file.Close() }()
	raw := make([]byte, 32)
	if _, err := io.ReadFull(file, raw); err != nil {
		return header, err
	}
	header.Magic = binary.BigEndian.Uint32(raw[0:4])
	if header.Magic != w8WALMagicBE && header.Magic != w8WALMagicLE {
		return header, fmt.Errorf("wal header magic %#x is not a SQLite WAL", header.Magic)
	}
	header.Present = true
	header.Format = binary.BigEndian.Uint32(raw[4:8])
	header.PageSize = binary.BigEndian.Uint32(raw[8:12])
	header.CheckpointSeq = binary.BigEndian.Uint32(raw[12:16])
	header.Salt1 = binary.BigEndian.Uint32(raw[16:20])
	header.Salt2 = binary.BigEndian.Uint32(raw[20:24])
	return header, nil
}

// w8CheckpointCounter turns a series of WAL-header samples into a count of WAL
// resets — the externally observable form of "a checkpoint restarted the log",
// since a successful checkpoint emits no log line and no in-process counter
// (map-e2e-io.md §4.5, §5.3).
//
// A PASSIVE checkpoint that does not restart the log is invisible here by
// construction, so this is a lower bound and is reported as "wal_resets", never
// as "checkpoints".
type w8CheckpointCounter struct {
	last   w8WALHeader
	seeded bool
	resets int
}

func (c *w8CheckpointCounter) Observe(header w8WALHeader) {
	defer func() { c.last, c.seeded = header, true }()
	if !c.seeded || !c.last.Present {
		return
	}
	if !header.Present {
		// The log was truncated away: a TRUNCATE checkpoint.
		c.resets++
		return
	}
	if header.CheckpointSeq != c.last.CheckpointSeq || header.Salt1 != c.last.Salt1 {
		c.resets++
	}
}

func (c *w8CheckpointCounter) Resets() int { return c.resets }

// --------------------------------------------------------------- sampler ---

// w8FileSizes is the store-side series, taken with os.Stat so sampling can
// never perturb the writer with a second SQLite connection.
type w8FileSizes struct {
	Store, WAL, SHM, Log int64
}

// w8Sample is one line of samples.ndjson.
type w8Sample struct {
	T     string `json:"t"`
	Phase string `json:"phase"`
	// Window names the sub-window of the phase this sample was taken in, or
	// "" outside one. A phase that performs its own stimulus — a commit, a
	// track, a poll — needs the series cut where the stimulus was, otherwise
	// the phase's name is a claim about work the phase did not do.
	Window           string  `json:"window,omitempty"`
	ElapsedSeconds   float64 `json:"elapsed_s"`
	PID              int     `json:"pid"`
	LogicalWrites    *uint64 `json:"ri_logical_writes,omitempty"`
	DiskBytesWritten uint64  `json:"ri_diskio_byteswritten"`
	DiskBytesRead    uint64  `json:"ri_diskio_bytesread,omitempty"`
	PhysFootprint    uint64  `json:"ri_phys_footprint,omitempty"`
	StoreBytes       int64   `json:"store_bytes"`
	WALBytes         int64   `json:"wal_bytes"`
	SHMBytes         int64   `json:"shm_bytes"`
	LogBytes         int64   `json:"log_bytes"`
	WALPresent       bool    `json:"wal_present"`
	WALCheckpointSeq uint32  `json:"wal_ckpt_seq"`
	WALSalt1         uint32  `json:"wal_salt1"`
	WALResets        int     `json:"wal_resets"`
	// CheckpointBytes is the running total of logical writes this sampler
	// attributed to WAL checkpoints: the logical-write delta of every sample
	// on which wal_resets moved. It is a derived series and is written into
	// the stream so the same reduction can be recomputed offline from the
	// samples of a run whose report predates the field.
	CheckpointBytes uint64 `json:"wal_checkpoint_bytes_cumulative,omitempty"`
	Error           string `json:"error,omitempty"`
	// Unwired names every reader this sampler was never given. A sampler with
	// a missing wiring cannot produce the series that reader feeds, and the
	// only honest report of that is the reader's name — not the zero value the
	// series would otherwise carry, which is indistinguishable from a real
	// measurement of zero.
	Unwired []string `json:"unwired_readers,omitempty"`
}

// w8Sampler writes one w8Sample per tick to an ndjson stream. The stream must
// live outside the daemon's private root, so the sampler's own bytes are never
// counted as fixture writes; w8ArtifactPath enforces that.
type w8Sampler struct {
	interval time.Duration
	now      func() time.Time
	readIO   func() (issue767ProcessIO, error)
	readSize func() w8FileSizes
	readWAL  func() (w8WALHeader, error)
	pid      func() int

	mu      sync.Mutex
	out     io.Writer
	start   time.Time
	phase   string
	window  string
	samples int
	counter w8CheckpointCounter
	failed  int

	// The checkpoint-bytes derivation, carried across samples: the previous
	// sample's cumulative logical writes and reset count. A sample on which
	// the reset count moved had a checkpoint inside its interval, so its
	// logical-write delta is booked to the checkpoint rather than to the
	// phase's own work.
	lastLogical     uint64
	haveLastLogical bool
	lastResets      int
	checkpointBytes uint64
}

// newW8Sampler builds a sampler with no readers. Every reader is wired by the
// caller (w8NewRunSampler in production, the unit tests here with fakes), and
// an unwired one is reported by name in every sample it touches rather than
// defaulted.
//
// The defaults it does NOT have are the point. A silent default is worse than
// no reader at all: `readWAL` returning a zero header and a nil error made the
// checkpoint counter observe nothing, so `wal_resets` read 0 for a whole run
// while `sample_failures` stayed 0 and the series reported itself healthy —
// the shape in which a deleted production wiring becomes a measurement of
// zero. The same held for the store/WAL/SHM/log sizes and the PID.
func newW8Sampler(out io.Writer, interval time.Duration) *w8Sampler {
	if interval <= 0 {
		interval = time.Second
	}
	return &w8Sampler{
		interval: interval,
		now:      time.Now,
		out:      out,
		phase:    "init",
	}
}

func (s *w8Sampler) SetPhase(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase, s.window = phase, ""
}

// SetWindow labels every subsequent sample with a sub-window of the current
// phase. SetPhase clears it, so a window can never leak past the phase that
// opened it.
func (s *w8Sampler) SetWindow(window string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.window = window
}

// Resets is the running WAL-reset count; Samples and Failures describe the
// series' own health so a phase with a broken sampler is visible as such.
func (s *w8Sampler) Resets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counter.Resets()
}

func (s *w8Sampler) Samples() (samples, failures int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples, s.failed
}

// CheckpointBytes is the running total of logical writes booked to WAL
// checkpoints. A window's cost is the difference of two readings, exactly as
// Resets is.
func (s *w8Sampler) CheckpointBytes() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpointBytes
}

// Sample takes and writes exactly one sample. Run calls it on a ticker; a
// caller may also take a sample at a phase boundary.
func (s *w8Sampler) Sample() w8Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.start.IsZero() {
		s.start = s.now()
	}
	var unwired []string
	sizes := w8FileSizes{}
	if s.readSize != nil {
		sizes = s.readSize()
	} else {
		unwired = append(unwired, "readSize")
	}
	pid := 0
	if s.pid != nil {
		pid = s.pid()
	} else {
		unwired = append(unwired, "pid")
	}
	sample := w8Sample{
		T:              s.now().UTC().Format(time.RFC3339Nano),
		Phase:          s.phase,
		Window:         s.window,
		ElapsedSeconds: s.now().Sub(s.start).Seconds(),
		PID:            pid,
		StoreBytes:     sizes.Store,
		WALBytes:       sizes.WAL,
		SHMBytes:       sizes.SHM,
		LogBytes:       sizes.Log,
	}
	if s.readWAL == nil {
		unwired = append(unwired, "readWAL")
	} else if header, err := s.readWAL(); err != nil {
		sample.Error = err.Error()
	} else {
		s.counter.Observe(header)
		sample.WALPresent, sample.WALCheckpointSeq, sample.WALSalt1 = header.Present, header.CheckpointSeq, header.Salt1
	}
	if s.readIO == nil {
		unwired = append(unwired, "readIO")
	} else if usage, err := s.readIO(); err != nil {
		sample.Error = strings.TrimSpace(sample.Error + " " + err.Error())
	} else {
		sample.DiskBytesWritten, sample.DiskBytesRead = usage.BytesWritten, usage.BytesRead
		sample.PhysFootprint = usage.PhysFootprint
		sample.LogicalWrites = usage.LogicalBytesWritten
	}
	sample.WALResets = s.counter.Resets()
	// Book this interval's logical writes to the checkpoint when the log was
	// restarted inside it. The attribution is per sample interval, so it is as
	// coarse as the sample rate: a phase whose own work runs concurrently with
	// a drain has that work booked to the drain too. That coarseness is why
	// this series is reported beside the total rather than in place of it, and
	// why only the low-activity phases are judged on it.
	if sample.LogicalWrites == nil {
		// A sample with no reading breaks the chain: the next delta would span
		// a gap nobody measured, and a gap is not a checkpoint.
		s.haveLastLogical = false
	} else {
		if s.haveLastLogical && sample.WALResets > s.lastResets && *sample.LogicalWrites > s.lastLogical {
			s.checkpointBytes += *sample.LogicalWrites - s.lastLogical
		}
		s.lastLogical, s.haveLastLogical = *sample.LogicalWrites, true
	}
	s.lastResets = sample.WALResets
	sample.CheckpointBytes = s.checkpointBytes
	// A missing wiring is an unavailability with a name, and it counts against
	// the series' own health exactly like a failed read: a phase whose sampler
	// was never wired must not present itself as a phase that measured zero.
	if len(unwired) > 0 {
		sample.Unwired = unwired
		sample.Error = strings.TrimSpace(sample.Error + " sampler readers not wired: " + strings.Join(unwired, ","))
	}
	if sample.Error != "" {
		s.failed++
	}
	s.samples++
	if s.out != nil {
		line, err := json.Marshal(sample)
		if err == nil {
			_, _ = s.out.Write(append(line, '\n'))
		}
	}
	return sample
}

// Run samples until the context is cancelled.
func (s *w8Sampler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.Sample()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sample()
		}
	}
}

// w8ArtifactPath joins an artifact file under artifactDir and refuses any
// destination inside the daemon's private root: an artifact written into the
// measured tree would be counted as the daemon's own write.
func w8ArtifactPath(artifactDir, daemonRoot, name string) (string, error) {
	dir, err := filepath.Abs(artifactDir)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(daemonRoot)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, dir)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact dir %s is inside the measured daemon root %s", dir, root)
	}
	return filepath.Join(dir, name), nil
}

// ------------------------------------------------------- writer attribution ---

// Destination buckets. Every file under the private root lands in exactly one;
// anything unrecognised lands in w8BucketUnclassified and is listed by name and
// size, so ri_logical_writes can be reconciled against named destinations
// rather than against a residue nobody looked at.
// w8StoreRel is the measured store's path relative to the private root. The
// fixture always puts it there (issue767Fixture.store), and attribution is
// anchored on it rather than on a "*.sqlite" suffix: the daemon keeps several
// OTHER SQLite databases under the same root (data/gortex/sidecar.sqlite,
// data/gortex/memories/sidecar.sqlite, a notebook cache), and folding their
// WAL traffic into the measured store's line would overstate exactly the number
// this harness exists to report.
const w8StoreRel = "store.sqlite"

const (
	w8BucketStore        = "store"
	w8BucketStoreWAL     = "store_wal"
	w8BucketStoreSHM     = "store_shm"
	w8BucketStoreLock    = "store_lock"
	w8BucketSidecar      = "sidecar_db"
	w8BucketModel        = "embedding_model"
	w8BucketDaemonLog    = "daemon_log"
	w8BucketDaemonPID    = "daemon_pid"
	w8BucketDaemonSocket = "daemon_socket"
	w8BucketDaemonState  = "daemon_state"
	w8BucketSnapshot     = "daemon_snapshot"
	w8BucketStopIntent   = "stop_intent"
	w8BucketTelemetry    = "telemetry"
	w8BucketGitignore    = "gitignore"
	w8BucketConfig       = "config"
	w8BucketQueryLog     = "query_log"
	w8BucketCache        = "cache"
	w8BucketData         = "data"
	w8BucketFixtureGit   = "fixture_git"
	w8BucketFixtureSrc   = "fixture_source"
	w8BucketArtifact     = "harness_artifact"
	w8BucketUnclassified = "unclassified"
)

// w8ClassifyPath names the writer of one path relative to the private root.
func w8ClassifyPath(rel string) string {
	rel = filepath.ToSlash(rel)
	base := rel[strings.LastIndexByte(rel, '/')+1:]
	switch {
	case rel == w8StoreRel:
		return w8BucketStore
	case rel == w8StoreRel+"-wal":
		return w8BucketStoreWAL
	case rel == w8StoreRel+"-shm":
		return w8BucketStoreSHM
	case strings.HasPrefix(rel, w8StoreRel+".") && strings.HasSuffix(base, ".lock"):
		return w8BucketStoreLock
	case strings.Contains(base, ".sqlite"):
		// Every other SQLite file under the root: the daemon's sidecars.
		return w8BucketSidecar
	case strings.HasPrefix(base, "query-log"):
		// internal/mcp/query_log.go:126 puts query-log.jsonl under the cache
		// dir. Folding it into `cache` hid a per-call write behind a bucket
		// named for something disposable, which is exactly the attribution an
		// idle floor has to be read off.
		return w8BucketQueryLog
	case strings.Contains(rel, "/models/"):
		// The embedding model is materialised under the data dir even with
		// --embeddings=false; tens of megabytes that are not store traffic.
		return w8BucketModel
	case base == "daemon.pid":
		return w8BucketDaemonPID
	case base == "daemon.sock" || strings.HasSuffix(base, ".sock"):
		return w8BucketDaemonSocket
	case base == "daemon.stopped":
		return w8BucketStopIntent
	case strings.HasPrefix(base, "daemon.state"):
		return w8BucketDaemonState
	case base == "daemon.gob.gz" || strings.HasPrefix(base, "daemon.snapshot"):
		return w8BucketSnapshot
	case strings.HasPrefix(base, "daemon") && strings.HasSuffix(base, ".log"):
		return w8BucketDaemonLog
	case base == "consent.json" || base == "install-id" || base == "last-send" ||
		strings.HasPrefix(base, "rollup-") || strings.Contains(rel, "/telemetry/"):
		return w8BucketTelemetry
	case base == ".gitignore":
		return w8BucketGitignore
	case strings.HasPrefix(rel, ".git/") || strings.Contains(rel, "/.git/") || base == ".git":
		// base == ".git" is a linked worktree's gitlink file.
		return w8BucketFixtureGit
	case rel == "gitconfig":
		return w8BucketFixtureGit
	case strings.HasPrefix(rel, "config/"):
		return w8BucketConfig
	case strings.HasSuffix(base, ".ndjson") || strings.HasSuffix(base, ".manifest.json"):
		return w8BucketArtifact
	case strings.HasSuffix(base, ".go") || base == "go.mod" || base == "go.sum":
		return w8BucketFixtureSrc
	case strings.HasPrefix(rel, "cache/"):
		return w8BucketCache
	case strings.HasPrefix(rel, "data/"):
		return w8BucketData
	default:
		return w8BucketUnclassified
	}
}

type w8CensusEntry struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// w8Census is the on-disk destination census of one private root.
type w8Census struct {
	TotalBytes   int64            `json:"total_bytes"`
	Bytes        map[string]int64 `json:"bytes_by_writer"`
	Files        map[string]int   `json:"files_by_writer"`
	Unclassified []w8CensusEntry  `json:"unclassified,omitempty"`
}

func w8WalkCensus(root string) (w8Census, error) {
	census := w8Census{Bytes: map[string]int64{}, Files: map[string]int{}}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A file the daemon deleted mid-walk is not a census failure.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		bucket := w8ClassifyPath(rel)
		census.Bytes[bucket] += info.Size()
		census.Files[bucket]++
		census.TotalBytes += info.Size()
		if bucket == w8BucketUnclassified {
			census.Unclassified = append(census.Unclassified, w8CensusEntry{Path: filepath.ToSlash(rel), Bytes: info.Size()})
		}
		return nil
	})
	sort.Slice(census.Unclassified, func(i, j int) bool {
		return census.Unclassified[i].Path < census.Unclassified[j].Path
	})
	return census, err
}

// ------------------------------------------------- checkpoint attribution ---

// w8SampleSeries is what a samples stream reduces to once the checkpoint
// intervals are separated from the rest: bytes booked to a WAL checkpoint, per
// phase and per phase/window.
//
// The reduction is defined on the stream rather than only inside the running
// sampler on purpose: a run whose report predates the series — every artifact
// already frozen — still carries its samples.ndjson, and the same rule applied
// to those bytes yields the same numbers. A derived series that can only be
// produced live is a series nobody can check.
type w8SampleSeries struct {
	// Phases maps phase name to bytes booked to checkpoints inside it.
	Phases map[string]uint64 `json:"phases"`
	// Windows maps "<phase>/<window>" to the same, for the sub-windows a
	// phase cut its own stimulus into.
	Windows map[string]uint64 `json:"windows,omitempty"`
	// Samples and Attributed say how much of the stream the rule saw and how
	// many intervals it booked, so a zero can be told apart from a series the
	// rule never got to look at.
	Samples    int `json:"samples"`
	Attributed int `json:"attributed_intervals"`
}

// w8WindowKey is the name a sub-window is reported under.
func w8WindowKey(phase, window string) string { return phase + "/" + window }

// w8CheckpointSeries books each sample interval on which wal_resets moved to
// that sample's phase (and window). A sample with no logical-writes reading
// breaks the chain: the next interval would span a gap nobody measured.
func w8CheckpointSeries(samples []w8Sample) w8SampleSeries {
	series := w8SampleSeries{Phases: map[string]uint64{}, Windows: map[string]uint64{}, Samples: len(samples)}
	var lastLogical uint64
	haveLast := false
	lastResets := 0
	seeded := false
	for _, sample := range samples {
		if sample.LogicalWrites == nil {
			haveLast = false
			lastResets, seeded = sample.WALResets, true
			continue
		}
		if haveLast && seeded && sample.WALResets > lastResets && *sample.LogicalWrites > lastLogical {
			delta := *sample.LogicalWrites - lastLogical
			series.Phases[sample.Phase] += delta
			if sample.Window != "" {
				series.Windows[w8WindowKey(sample.Phase, sample.Window)] += delta
			}
			series.Attributed++
		}
		lastLogical, haveLast = *sample.LogicalWrites, true
		lastResets, seeded = sample.WALResets, true
	}
	return series
}

// w8ReadSamples parses a samples.ndjson stream. A truncated last line is
// tolerated — a run killed mid-sample still has every sample before it — and
// reported by count rather than swallowed.
func w8ReadSamples(path string) ([]w8Sample, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var samples []w8Sample
	skipped := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var sample w8Sample
		if err := json.Unmarshal([]byte(line), &sample); err != nil {
			skipped++
			continue
		}
		samples = append(samples, sample)
	}
	return samples, skipped, nil
}

// -------------------------------------------------------------- manifest ---

// w8Manifest is what makes a number reproducible: which binary, built from
// which tree, on which toolchain, against which fixture, with which
// environment. A report without one is an anecdote.
type w8Manifest struct {
	RunID         string            `json:"run_id"`
	Arm           string            `json:"arm"`
	StartedAt     string            `json:"started_at"`
	Binary        string            `json:"binary"`
	BinarySHA256  string            `json:"binary_sha256"`
	BinaryVersion string            `json:"binary_version,omitempty"`
	SourceCommit  string            `json:"source_commit,omitempty"`
	DirtyDigest   string            `json:"dirty_tree_digest,omitempty"`
	Toolchain     string            `json:"toolchain"`
	GOOS          string            `json:"goos"`
	GOARCH        string            `json:"goarch"`
	Python        string            `json:"python,omitempty"`
	Fixture       w8FixtureSpec     `json:"fixture"`
	FixtureDigest string            `json:"fixture_digest"`
	Worktrees     int               `json:"worktrees"`
	Commits       int               `json:"commits"`
	Edits         int               `json:"edits"`
	Repetitions   int               `json:"repetitions"`
	Cold          bool              `json:"cold"`
	Env           map[string]string `json:"env"`
	Notes         []string          `json:"notes,omitempty"`
}

func w8FileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// w8RecordedEnv keeps the variables that change what is measured and drops
// everything else. Anything whose name looks like a credential is dropped even
// when its prefix is on the list: a manifest is an artifact that gets copied
// around.
func w8RecordedEnv(environ []string) map[string]string {
	prefixes := []string{"GXW8_", "GORTEX_", "XDG_", "GIT_", "GO", "TMPDIR", "HOME", "PATH", "LANG", "LC_", "CI", "NO_COLOR"}
	secrets := []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH"}
	recorded := map[string]string{}
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		keep := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(upper, prefix) {
				keep = true
				break
			}
		}
		if !keep {
			continue
		}
		for _, secret := range secrets {
			if strings.Contains(upper, secret) {
				keep = false
				break
			}
		}
		if keep {
			recorded[key] = value
		}
	}
	return recorded
}

// w8BuildManifest fills in everything derivable from the machine. Fields the
// caller owns (RunID, Arm, Fixture, Cold, Notes …) are taken from the template.
func w8BuildManifest(template w8Manifest, binary string, environ []string, fixture []w8FixtureFile) w8Manifest {
	manifest := template
	manifest.StartedAt = time.Now().UTC().Format(time.RFC3339)
	manifest.Binary = binary
	if sum, err := w8FileSHA256(binary); err == nil {
		manifest.BinarySHA256 = sum
	} else {
		manifest.Notes = append(manifest.Notes, "binary sha256 unavailable: "+err.Error())
	}
	manifest.Toolchain = runtime.Version()
	manifest.GOOS, manifest.GOARCH = runtime.GOOS, runtime.GOARCH
	manifest.Env = w8RecordedEnv(environ)
	manifest.FixtureDigest = w8FixtureDigest(fixture)
	if path, err := exec.LookPath("python3"); err == nil {
		manifest.Python = path
	}
	return manifest
}

// ------------------------------------------------- daemon counter scraping ---

// w8ParseStatusCounters pulls views.counters out of `daemon status --format
// json`. A binary without the flag (every pre-W8.3 baseline arm) fails the
// command outright; the caller records the unavailability by name instead of
// substituting zeros, because a missing series and a zero series are different
// facts.
func w8ParseStatusCounters(output []byte) (map[string]int64, error) {
	var payload struct {
		Views *struct {
			Counters map[string]int64 `json:"counters"`
		} `json:"views"`
	}
	if err := json.Unmarshal(output, &payload); err != nil {
		return nil, fmt.Errorf("daemon status is not JSON: %w", err)
	}
	if payload.Views == nil {
		return nil, errors.New("daemon status carries no views block")
	}
	if payload.Views.Counters == nil {
		return nil, errors.New("daemon status views block carries no counters")
	}
	return payload.Views.Counters, nil
}

// w8CounterDelta subtracts two counter snapshots, keeping every series either
// side names.
func w8CounterDelta(before, after map[string]int64) map[string]int64 {
	delta := map[string]int64{}
	for key, value := range after {
		if d := value - before[key]; d != 0 {
			delta[key] = d
		}
	}
	for key, value := range before {
		if _, ok := after[key]; !ok && value != 0 {
			delta[key] = -value
		}
	}
	return delta
}

// ----------------------------------------------------------------- tests ---

func TestW8ReadWALHeaderParsesTheDocumentedLayout(t *testing.T) {
	raw := make([]byte, 64)
	binary.BigEndian.PutUint32(raw[0:4], w8WALMagicBE)
	binary.BigEndian.PutUint32(raw[4:8], 3007000)
	binary.BigEndian.PutUint32(raw[8:12], 4096)
	binary.BigEndian.PutUint32(raw[12:16], 9)
	binary.BigEndian.PutUint32(raw[16:20], 0xaabbccdd)
	binary.BigEndian.PutUint32(raw[20:24], 0x11223344)
	path := filepath.Join(t.TempDir(), "store.sqlite-wal")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	header, err := w8ReadWALHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if !header.Present || header.PageSize != 4096 || header.CheckpointSeq != 9 ||
		header.Salt1 != 0xaabbccdd || header.Salt2 != 0x11223344 || header.Bytes != 64 {
		t.Fatalf("parsed %+v", header)
	}
}

func TestW8ReadWALHeaderTreatsAbsentAndTruncatedAsNotPresent(t *testing.T) {
	dir := t.TempDir()
	header, err := w8ReadWALHeader(filepath.Join(dir, "missing-wal"))
	if err != nil || header.Present {
		t.Fatalf("absent wal: header=%+v err=%v", header, err)
	}
	empty := filepath.Join(dir, "empty-wal")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	header, err = w8ReadWALHeader(empty)
	if err != nil || header.Present {
		t.Fatalf("truncated wal: header=%+v err=%v", header, err)
	}
	foreign := filepath.Join(dir, "foreign-wal")
	if err := os.WriteFile(foreign, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w8ReadWALHeader(foreign); err == nil {
		t.Fatal("a foreign magic must be an error, not a silent zero header")
	}
}

func TestW8CheckpointCounterCountsResetsNotAppends(t *testing.T) {
	present := func(seq, salt uint32) w8WALHeader {
		return w8WALHeader{Present: true, CheckpointSeq: seq, Salt1: salt}
	}
	var counter w8CheckpointCounter
	for _, header := range []w8WALHeader{
		{},                 // no WAL yet
		present(0, 0x1111), // WAL created: not a reset
		present(0, 0x1111), // appended to: not a reset
		present(1, 0x2222), // checkpoint restarted the log
		present(1, 0x2222),
		{},                 // truncate checkpoint removed the log
		present(2, 0x3333), // recreated after the truncate: not a reset
		present(3, 0x3333), // sequence moved with a stable salt: still a reset
	} {
		counter.Observe(header)
	}
	if got := counter.Resets(); got != 3 {
		t.Fatalf("counted %d resets, want 3", got)
	}
}

// TestW8WALCheckpointSequenceAdvancesOnReset is the experiment map-e2e-io.md
// §5.3 asks for before the harness relies on the header field: does the WAL
// checkpoint-sequence at offset 12 actually move under modernc.org/sqlite?
// The answer is recorded in the test's own failure text so a future change of
// driver is caught here rather than silently zeroing a measured series.
func TestW8WALCheckpointSequenceAdvancesOnReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Skipf("driver refused WAL journal mode (got %q); the harness falls back to the daemon's own counters", mode)
	}
	if _, err := db.Exec("CREATE TABLE probe(id INTEGER PRIMARY KEY, payload BLOB)"); err != nil {
		t.Fatal(err)
	}
	fill := func(rows int) {
		t.Helper()
		for i := 0; i < rows; i++ {
			if _, err := db.Exec("INSERT INTO probe(payload) VALUES (?)", bytes.Repeat([]byte("w8"), 512)); err != nil {
				t.Fatal(err)
			}
		}
	}
	fill(64)
	before, err := w8ReadWALHeader(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if !before.Present {
		t.Skip("no WAL file materialised; the harness falls back to the daemon's own counters")
	}
	var busy, walFrames, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(RESTART)").Scan(&busy, &walFrames, &checkpointed); err != nil {
		t.Fatal(err)
	}
	fill(8)
	after, err := w8ReadWALHeader(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wal_checkpoint(RESTART): busy=%d frames=%d checkpointed=%d; header ckpt_seq %d -> %d, salt1 %#x -> %#x",
		busy, walFrames, checkpointed, before.CheckpointSeq, after.CheckpointSeq, before.Salt1, after.Salt1)
	var counter w8CheckpointCounter
	counter.Observe(before)
	counter.Observe(after)
	if counter.Resets() != 1 {
		t.Fatalf("a RESTART checkpoint did not move the WAL header (ckpt_seq %d -> %d, salt1 %#x -> %#x); "+
			"the harness must not derive checkpoint counts from the header on this driver",
			before.CheckpointSeq, after.CheckpointSeq, before.Salt1, after.Salt1)
	}
}

func TestW8SamplerWritesOneLinePerTickWithPhaseLabels(t *testing.T) {
	var out bytes.Buffer
	sampler := newW8Sampler(&out, time.Millisecond)
	logical := uint64(4096)
	clock := time.Unix(1_700_000_000, 0)
	sampler.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	sampler.pid = func() int { return 4242 }
	sampler.readIO = func() (issue767ProcessIO, error) {
		logical += 1024
		value := logical
		return issue767ProcessIO{BytesWritten: logical / 2, LogicalBytesWritten: &value, PhysFootprint: 99}, nil
	}
	sampler.readSize = func() w8FileSizes { return w8FileSizes{Store: 10, WAL: 20, SHM: 30, Log: 40} }
	seq := uint32(0)
	sampler.readWAL = func() (w8WALHeader, error) {
		seq++
		return w8WALHeader{Present: true, CheckpointSeq: seq, Salt1: seq}, nil
	}
	sampler.SetPhase("P0_cold_index")
	sampler.Sample()
	sampler.SetPhase("P1_idle_cold")
	sampler.Sample()
	sampler.Sample()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("wrote %d lines, want 3:\n%s", len(lines), out.String())
	}
	phases := []string{"P0_cold_index", "P1_idle_cold", "P1_idle_cold"}
	for i, line := range lines {
		var sample w8Sample
		if err := json.Unmarshal([]byte(line), &sample); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if sample.Phase != phases[i] {
			t.Errorf("line %d phase = %q, want %q", i, sample.Phase, phases[i])
		}
		if sample.PID != 4242 || sample.StoreBytes != 10 || sample.WALBytes != 20 || sample.LogBytes != 40 {
			t.Errorf("line %d lost a series: %+v", i, sample)
		}
		if sample.LogicalWrites == nil {
			t.Errorf("line %d dropped the primary logical-write series", i)
		}
		if sample.Error != "" {
			t.Errorf("line %d reported %q", i, sample.Error)
		}
	}
	if got := sampler.Resets(); got != 2 {
		t.Errorf("sampler counted %d WAL resets, want 2", got)
	}
	if samples, failures := sampler.Samples(); samples != 3 || failures != 0 {
		t.Errorf("sampler health = %d samples / %d failures, want 3/0", samples, failures)
	}
}

func TestW8SamplerRecordsReaderFailuresInsteadOfDroppingTheSample(t *testing.T) {
	var out bytes.Buffer
	sampler := newW8Sampler(&out, time.Millisecond)
	sampler.readIO = func() (issue767ProcessIO, error) {
		return issue767ProcessIO{}, errors.New("proc_pid_rusage: no such process")
	}
	sampler.Sample()
	var sample w8Sample
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &sample); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sample.Error, "no such process") {
		t.Fatalf("failure not recorded: %+v", sample)
	}
	if _, failures := sampler.Samples(); failures != 1 {
		t.Fatal("a failed read must count against sampler health")
	}
	// The three readers this test never wired are named, not defaulted.
	if len(sample.Unwired) != 3 {
		t.Fatalf("a sampler with one wired reader named %v as unwired, want the other three", sample.Unwired)
	}
}

// TestW8SamplerNamesEveryUnwiredReaderInsteadOfReportingZero is the guard
// behind the w8NewRunSampler wiring pin: whatever else changes, a reader that
// is not wired must be named in the sample and must cost the series its health,
// so a deleted production wiring can never present itself as a measurement.
func TestW8SamplerNamesEveryUnwiredReaderInsteadOfReportingZero(t *testing.T) {
	var out bytes.Buffer
	sampler := newW8Sampler(&out, time.Hour)
	sample := sampler.Sample()

	want := map[string]bool{"readSize": true, "pid": true, "readWAL": true, "readIO": true}
	if len(sample.Unwired) != len(want) {
		t.Fatalf("a bare sampler named %v, want all of %v", sample.Unwired, want)
	}
	for _, name := range sample.Unwired {
		if !want[name] {
			t.Fatalf("unexpected unwired reader %q", name)
		}
		delete(want, name)
	}
	for _, field := range []struct {
		name string
		zero bool
	}{
		{"store_bytes", sample.StoreBytes == 0},
		{"wal_bytes", sample.WALBytes == 0},
		{"log_bytes", sample.LogBytes == 0},
		{"pid", sample.PID == 0},
		{"wal_present", !sample.WALPresent},
		{"ri_logical_writes", sample.LogicalWrites == nil},
	} {
		if !field.zero {
			t.Fatalf("an unwired sampler produced a value for %s: %+v", field.name, sample)
		}
	}
	// The zeros above are exactly why the naming has to exist: without it they
	// are indistinguishable from a real quiet phase.
	for _, name := range []string{"readSize", "pid", "readWAL", "readIO"} {
		if !strings.Contains(sample.Error, name) {
			t.Fatalf("sample error %q does not name the unwired reader %q", sample.Error, name)
		}
	}
	if _, failures := sampler.Samples(); failures != 1 {
		t.Fatal("an unwired sampler must count the sample against its own health")
	}
	// wal_resets must stay a lower bound that never advanced, not a count.
	if sampler.Resets() != 0 {
		t.Fatalf("an unwired sampler counted %d WAL resets", sampler.Resets())
	}
	var decoded w8Sample
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Unwired) != 4 {
		t.Fatalf("the written sample line lost the unavailability: %s", out.String())
	}
}

func TestW8SamplerRunTicksUntilCancelled(t *testing.T) {
	var out bytes.Buffer
	sampler := newW8Sampler(&out, 2*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sampler.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for {
		if samples, _ := sampler.Samples(); samples >= 3 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("sampler did not tick")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestW8ArtifactPathRefusesADestinationInsideTheMeasuredRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := w8ArtifactPath(filepath.Join(root, "artifacts"), root, "samples.ndjson"); err == nil {
		t.Fatal("an artifact dir inside the daemon root must be refused")
	}
	if _, err := w8ArtifactPath(root, root, "samples.ndjson"); err == nil {
		t.Fatal("the daemon root itself must be refused")
	}
	outside := t.TempDir()
	path, err := w8ArtifactPath(outside, root, "samples.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != outside {
		t.Fatalf("artifact path %s is not under %s", path, outside)
	}
}

func TestW8ClassifyPathAttributesEveryKnownWriter(t *testing.T) {
	for _, tc := range []struct{ rel, want string }{
		{"store.sqlite", w8BucketStore},
		{"store.sqlite-wal", w8BucketStoreWAL},
		{"store.sqlite-shm", w8BucketStoreSHM},
		{"store.sqlite.lock", w8BucketStoreLock},
		{"data/gortex/sidecar.sqlite", w8BucketSidecar},
		{"data/gortex/sidecar.sqlite-wal", w8BucketSidecar},
		{"data/gortex/memories/sidecar.sqlite-shm", w8BucketSidecar},
		{"data/gortex/notebook-cache/.gortex/sidecar.sqlite", w8BucketSidecar},
		{"data/gortex/models/potion-code-16M-v2/model.safetensors", w8BucketModel},
		{"data/gortex/models/potion-code-16M-v2/tokenizer.json", w8BucketModel},
		{"wt01/.git", w8BucketFixtureGit},
		{"daemon-1.log", w8BucketDaemonLog},
		{"cache/gortex/daemon.pid", w8BucketDaemonPID},
		{"cache/gortex/daemon.sock", w8BucketDaemonSocket},
		{"cache/gortex/daemon.stopped", w8BucketStopIntent},
		{"cache/gortex/daemon.state.json", w8BucketDaemonState},
		{"cache/gortex/daemon.gob.gz", w8BucketSnapshot},
		{"data/gortex/consent.json", w8BucketTelemetry},
		{"data/gortex/install-id", w8BucketTelemetry},
		{"data/gortex/last-send", w8BucketTelemetry},
		{"data/gortex/telemetry/rollup-2026-09-13.json", w8BucketTelemetry},
		{"data/gortex/store/.gitignore", w8BucketGitignore},
		{"config/gortex/config.yaml", w8BucketConfig},
		{"repo/.git/index", w8BucketFixtureGit},
		{"gitconfig", w8BucketFixtureGit},
		{"repo/p001/file00001.go", w8BucketFixtureSrc},
		{"repo/go.mod", w8BucketFixtureSrc},
		{"cache/gortex/searcher.bin", w8BucketCache},
		{"data/gortex/memories.db", w8BucketData},
		{"something/unexpected.bin", w8BucketUnclassified},
	} {
		if got := w8ClassifyPath(tc.rel); got != tc.want {
			t.Errorf("w8ClassifyPath(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

func TestW8WalkCensusListsUnclassifiedBytesByName(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, size int) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("store.sqlite", 100)
	write("store.sqlite-wal", 50)
	write("daemon-1.log", 25)
	write("repo/p000/file00000.go", 10)
	write("mystery.bin", 7)
	write("nested/other.dat", 3)

	census, err := w8WalkCensus(root)
	if err != nil {
		t.Fatal(err)
	}
	if census.TotalBytes != 195 {
		t.Errorf("total = %d, want 195", census.TotalBytes)
	}
	if census.Bytes[w8BucketStore] != 100 || census.Bytes[w8BucketStoreWAL] != 50 || census.Bytes[w8BucketDaemonLog] != 25 {
		t.Errorf("store/wal/log attribution wrong: %+v", census.Bytes)
	}
	if census.Bytes[w8BucketUnclassified] != 10 || len(census.Unclassified) != 2 {
		t.Fatalf("unclassified residue not itemised: %+v %+v", census.Bytes, census.Unclassified)
	}
	if census.Unclassified[0].Path != "mystery.bin" || census.Unclassified[0].Bytes != 7 {
		t.Errorf("unclassified entries = %+v", census.Unclassified)
	}
	if census.Files[w8BucketFixtureSrc] != 1 {
		t.Errorf("file counts wrong: %+v", census.Files)
	}
}

func TestW8RecordedEnvKeepsKnobsAndDropsCredentials(t *testing.T) {
	env := w8RecordedEnv([]string{
		"GXW8_FIXTURE_FILES=1500",
		"GORTEX_RECONCILE_INTERVAL=5s",
		"GORTEX_TELEMETRY=0",
		"XDG_DATA_HOME=/tmp/x",
		"GOFLAGS=-mod=mod",
		"ANTHROPIC_API_KEY=secret",
		"GORTEX_LLM_ANTHROPIC_API_KEY=secret",
		"GITHUB_TOKEN=secret",
		"UNRELATED=1",
	})
	for _, want := range []string{"GXW8_FIXTURE_FILES", "GORTEX_RECONCILE_INTERVAL", "GORTEX_TELEMETRY", "XDG_DATA_HOME", "GOFLAGS"} {
		if _, ok := env[want]; !ok {
			t.Errorf("manifest dropped %s", want)
		}
	}
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "GORTEX_LLM_ANTHROPIC_API_KEY", "GITHUB_TOKEN", "UNRELATED"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("manifest recorded %s", forbidden)
		}
	}
}

func TestW8BuildManifestRecordsBinaryIdentity(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "gortex-fake")
	payload := []byte("not really a binary")
	if err := os.WriteFile(binary, payload, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := w8FixtureSpec{Files: 20, Packages: 4, Seed: 11}
	fixture := w8GenerateFixture(spec)
	manifest := w8BuildManifest(w8Manifest{RunID: "r1", Arm: "candidate", Fixture: spec.normalize(), Cold: true},
		binary, []string{"GXW8_REPS=1", "AWS_SECRET_ACCESS_KEY=x"}, fixture)
	want := sha256.Sum256(payload)
	if manifest.BinarySHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("binary sha256 = %q", manifest.BinarySHA256)
	}
	if manifest.FixtureDigest != w8FixtureDigest(fixture) {
		t.Error("fixture digest not recorded")
	}
	if manifest.Toolchain != runtime.Version() || manifest.GOOS != runtime.GOOS || manifest.GOARCH != runtime.GOARCH {
		t.Errorf("toolchain identity wrong: %+v", manifest)
	}
	if _, ok := manifest.Env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("manifest recorded a credential")
	}
	if manifest.Env["GXW8_REPS"] != "1" {
		t.Error("manifest dropped a harness knob")
	}
	if manifest.StartedAt == "" || manifest.RunID != "r1" || manifest.Arm != "candidate" || !manifest.Cold {
		t.Errorf("manifest template fields lost: %+v", manifest)
	}
}

func TestW8ParseStatusCountersReadsViewsBlock(t *testing.T) {
	counters, err := w8ParseStatusCounters([]byte(`{"views":{"counters":{"views_dedicated_base_claim_total|outcome=reused":3,"views_coordinator_cycle_total|outcome=built_commit":7}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if counters["views_dedicated_base_claim_total|outcome=reused"] != 3 || counters["views_coordinator_cycle_total|outcome=built_commit"] != 7 {
		t.Fatalf("counters = %+v", counters)
	}
	for _, tc := range []struct{ name, payload string }{
		{"text_output", "daemon: running\n"},
		{"no_views", `{"pid":1}`},
		{"no_counters", `{"views":{"families":2}}`},
	} {
		if _, err := w8ParseStatusCounters([]byte(tc.payload)); err == nil {
			t.Errorf("%s: expected a named unavailability, got none", tc.name)
		}
	}
}

func TestW8CounterDeltaKeepsSeriesFromBothSides(t *testing.T) {
	delta := w8CounterDelta(
		map[string]int64{"a": 1, "b": 5, "gone": 2},
		map[string]int64{"a": 4, "b": 5, "new": 9},
	)
	if delta["a"] != 3 || delta["new"] != 9 || delta["gone"] != -2 {
		t.Fatalf("delta = %+v", delta)
	}
	if _, ok := delta["b"]; ok {
		t.Fatalf("unchanged series must not appear: %+v", delta)
	}
}

// TestW8ProcessIOSamplerReadsThisProcess proves the measurement primitive
// itself works on this machine — the python3 + ctypes proc_pid_rusage shim on
// Darwin, /proc/<pid>/io on Linux — before any harness trusts its deltas.
func TestW8ProcessIOSamplerReadsThisProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process I/O sampler supports Darwin and Linux")
	}
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 is not on PATH; the Darwin rusage shim cannot run")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := issue767ReadProcessIO(ctx, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "burn.bin")
	if err := os.WriteFile(scratch, bytes.Repeat([]byte("w8"), 512*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	last, err := issue767ReadProcessIO(ctx, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if first.StartTicks != last.StartTicks {
		t.Fatalf("process identity changed between samples: %d -> %d", first.StartTicks, last.StartTicks)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	if first.LogicalBytesWritten == nil || last.LogicalBytesWritten == nil {
		t.Fatal("Darwin sampler did not report ri_logical_writes")
	}
	if *last.LogicalBytesWritten <= *first.LogicalBytesWritten {
		t.Fatalf("a 1 MiB write did not move ri_logical_writes: %d -> %d", *first.LogicalBytesWritten, *last.LogicalBytesWritten)
	}
	if last.PhysFootprint == 0 || last.UserTimeNS == 0 {
		t.Errorf("extended rusage fields are empty: %+v", last)
	}
}

// ----------------------------------------- checkpoint-attribution tests ---

// w8ScriptedSampler wires a sampler to a scripted series of process-IO and WAL
// readings, so the checkpoint attribution can be exercised without a daemon.
func w8ScriptedSampler(out io.Writer, logical []uint64, headers []w8WALHeader) *w8Sampler {
	sampler := newW8Sampler(out, time.Hour)
	tick := 0
	sampler.pid = func() int { return 4242 }
	sampler.readSize = func() w8FileSizes { return w8FileSizes{} }
	// Sample() reads the WAL header before the process counters, so the tick
	// advances on the LAST reader: both must describe the same instant.
	sampler.readWAL = func() (w8WALHeader, error) {
		return headers[min(tick, len(headers)-1)], nil
	}
	sampler.readIO = func() (issue767ProcessIO, error) {
		value := logical[min(tick, len(logical)-1)]
		tick++
		return issue767ProcessIO{LogicalBytesWritten: &value, StartTicks: 1}, nil
	}
	return sampler
}

// TestW8SamplerBooksOnlyTheCheckpointIntervalToTheCheckpointSeries is the
// derivation §F5(1) rests on: bytes written in the interval a WAL reset landed
// in are the checkpoint's; everything else is the phase's own work.
//
// The numbers are the frozen candidate_rep2 P2 row: 58,040,984 total, of which
// 42,607,016 is one drain, leaving 15,433,968 — 0.91x of the baseline's
// 16,879,664, inside the ceiling the total-series reading put it 3.44x outside.
func TestW8SamplerBooksOnlyTheCheckpointIntervalToTheCheckpointSeries(t *testing.T) {
	present := func(seq uint32) w8WALHeader { return w8WALHeader{Present: true, CheckpointSeq: seq, Salt1: 0x1111} }
	// Four intervals: work, work, the drain, work.
	logical := []uint64{0, 5_000_000, 10_000_000, 52_607_016, 58_040_984}
	headers := []w8WALHeader{present(0), present(0), present(0), present(1), present(1)}
	var out bytes.Buffer
	sampler := w8ScriptedSampler(&out, logical, headers)
	sampler.SetPhase("P2_small_edits")
	for range logical {
		sampler.Sample()
	}
	if got := sampler.CheckpointBytes(); got != 42_607_016 {
		t.Fatalf("checkpoint bytes = %d, want the 42,607,016 of the drain interval alone", got)
	}
	if got := sampler.Resets(); got != 1 {
		t.Fatalf("wal resets = %d, want 1", got)
	}
	total := logical[len(logical)-1]
	excluded := w8ExcludeCheckpoint(&total, sampler.CheckpointBytes())
	if excluded == nil || *excluded != 15_433_968 {
		t.Fatalf("excluded series = %v, want 15,433,968", excluded)
	}
	// The same rule applied offline to the stream it just wrote must agree:
	// the derived series is not allowed to exist only inside the process.
	samples, skipped, err := w8ReadSamplesFromString(out.String())
	if err != nil || skipped != 0 {
		t.Fatalf("stream re-read: err=%v skipped=%d", err, skipped)
	}
	series := w8CheckpointSeries(samples)
	if series.Phases["P2_small_edits"] != 42_607_016 || series.Attributed != 1 {
		t.Fatalf("offline reduction = %+v, want one 42,607,016 interval", series)
	}
}

// w8ReadSamplesFromString is w8ReadSamples over an in-memory stream.
func w8ReadSamplesFromString(stream string) ([]w8Sample, int, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("w8-samples-%d.ndjson", time.Now().UnixNano()))
	if err := os.WriteFile(path, []byte(stream), 0o600); err != nil {
		return nil, 0, err
	}
	defer func() { _ = os.Remove(path) }()
	return w8ReadSamples(path)
}

// TestW8CheckpointSeriesAttributesPerPhaseAndPerWindow pins the offline rule,
// including the sub-window attribution the P4 split depends on, and the gap
// rule: a sample with no reading breaks the chain rather than producing a
// delta that spans a hole.
func TestW8CheckpointSeriesAttributesPerPhaseAndPerWindow(t *testing.T) {
	value := func(v uint64) *uint64 { return &v }
	samples := []w8Sample{
		{Phase: "P4_amend_same_tree", Window: "P4a_commit_tree_change", LogicalWrites: value(100), WALResets: 0},
		{Phase: "P4_amend_same_tree", Window: "P4a_commit_tree_change", LogicalWrites: value(200), WALResets: 0},
		{Phase: "P4_amend_same_tree", Window: "P4a_commit_tree_change", LogicalWrites: value(900), WALResets: 1},
		{Phase: "P4_amend_same_tree", Window: "P4b_amend_same_tree", LogicalWrites: value(950), WALResets: 1},
		{Phase: "P4_amend_same_tree", Window: "P4b_amend_same_tree", LogicalWrites: nil, WALResets: 1},
		// The chain is broken, so this interval is not booked even though the
		// reset count moved: nobody measured the bytes it would claim.
		{Phase: "P4_amend_same_tree", Window: "P4b_amend_same_tree", LogicalWrites: value(9000), WALResets: 2},
		{Phase: "P8_idle_warm", LogicalWrites: value(9100), WALResets: 2},
		{Phase: "P8_idle_warm", LogicalWrites: value(9500), WALResets: 3},
	}
	series := w8CheckpointSeries(samples)
	if series.Phases["P4_amend_same_tree"] != 700 {
		t.Fatalf("P4 checkpoint bytes = %d, want the 700 of the reset interval", series.Phases["P4_amend_same_tree"])
	}
	if series.Windows[w8WindowKey("P4_amend_same_tree", "P4a_commit_tree_change")] != 700 {
		t.Fatalf("the commit window did not take the drain: %+v", series.Windows)
	}
	if _, ok := series.Windows[w8WindowKey("P4_amend_same_tree", "P4b_amend_same_tree")]; ok {
		t.Fatalf("the amend window was charged for a drain it did not carry: %+v", series.Windows)
	}
	if series.Phases["P8_idle_warm"] != 400 {
		t.Fatalf("P8 checkpoint bytes = %d, want 400", series.Phases["P8_idle_warm"])
	}
	if series.Attributed != 2 || series.Samples != len(samples) {
		t.Fatalf("series health = %+v", series)
	}
}

// TestW8SamplerWindowLabelsTheStreamAndClearsOnPhaseChange pins the sub-window
// label: a window may never outlive the phase that opened it, otherwise a
// later phase's samples would be attributed to a window that closed.
func TestW8SamplerWindowLabelsTheStreamAndClearsOnPhaseChange(t *testing.T) {
	var out bytes.Buffer
	sampler := w8ScriptedSampler(&out, []uint64{0, 1, 2}, []w8WALHeader{{Present: true}, {Present: true}, {Present: true}})
	sampler.SetPhase("P4_amend_same_tree")
	sampler.SetWindow(w8WindowCommitTreeChange)
	first := sampler.Sample()
	sampler.SetPhase("P5_main_advance")
	second := sampler.Sample()
	if first.Window != w8WindowCommitTreeChange {
		t.Fatalf("first sample window = %q", first.Window)
	}
	if second.Window != "" || second.Phase != "P5_main_advance" {
		t.Fatalf("the window leaked past its phase: %+v", second)
	}
}

// TestW8ClassifyPathSeparatesTheQueryLogFromTheCache pins the attribution
// correction §F5(3) needs: query-log.jsonl lives under the cache dir
// (internal/mcp/query_log.go:126) and was being counted as disposable cache,
// which is exactly the per-call write an idle floor has to be read off.
func TestW8ClassifyPathSeparatesTheQueryLogFromTheCache(t *testing.T) {
	for _, tc := range []struct{ rel, want string }{
		{"cache/gortex/query-log.jsonl", w8BucketQueryLog},
		{"cache/gortex/query-log.jsonl.1", w8BucketQueryLog},
		{"cache/gortex/searcher.bin", w8BucketCache},
		{"data/gortex/sidecar.sqlite-wal", w8BucketSidecar},
	} {
		if got := w8ClassifyPath(tc.rel); got != tc.want {
			t.Errorf("w8ClassifyPath(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}
