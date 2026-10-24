package indexer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

type contractColdDoorFixture struct {
	store      *store_sqlite.Store
	root       string
	configPath string
	mi         *MultiIndexer
	options    ContractFollowupCaptureOptions
	published  map[string]*atomic.Int64
	mu         sync.Mutex
}

const (
	coldDoorRoutes  = "package fixture\nfunc register(r Router) { r.GET(\"/before\", serve) }\nfunc serve() {}\n"
	coldDoorFailing = "package fixture\nfunc registerFailing(r Router) { r.GET(\"/failing\", failing) }\nfunc failing() {}\n"
)

func (f *contractColdDoorFixture) publishedFor(repo string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.published[repo] == nil {
		return 0
	}
	return f.published[repo].Load()
}

func (f *contractColdDoorFixture) hooks() ContractCoreRuntimeHooks {
	return ContractCoreRuntimeHooks{Published: func(_ context.Context, repo, _ string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.published[repo] == nil {
			f.published[repo] = &atomic.Int64{}
		}
		f.published[repo].Add(1)
	}}
}

// newContractColdDoorFixture tracks one repository through the multi-repo
// cold door on a fresh store, then certifies its baseline, so every later door
// starts from an accepted namespace.
func newContractColdDoorFixture(t *testing.T) *contractColdDoorFixture {
	t.Helper()
	ctx := t.Context()
	// The store alone: an Indexer with every language registered would be
	// built and dropped here, its query compilation most of the fixture's cost.
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	f := &contractColdDoorFixture{store: store, root: t.TempDir(), published: make(map[string]*atomic.Int64)}
	writeFile(t, filepath.Join(f.root, "routes.go"), coldDoorRoutes)
	writeFile(t, filepath.Join(f.root, "failing.go"), coldDoorFailing)
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "fixture", Path: f.root}}}
	f.configPath = filepath.Join(t.TempDir(), "config.yaml")
	cfg.SetConfigPath(f.configPath)
	require.NoError(t, cfg.Save())
	f.mi = f.newMultiIndexer(t)
	results, err := f.mi.IndexAll()
	require.NoError(t, err)
	require.Empty(t, results["fixture"].FailedFiles)
	state := f.state(t)
	require.False(t, state.Accepted, "a fresh namespace stays pending until its baseline certifies it")
	leases := graphview.NewLeaseManager()
	registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "fixture", RootIdentity: f.root, Incarnation: t.Name()}, nil)
	require.NoError(t, err)
	_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "cold-door-accepted-source")
	require.NoError(t, err)
	f.options = ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}, MultiIndexer: f.mi, Registry: f.mi.registry, Config: config.Default().Index, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
	// A never-mutated owner has no raw-source witness; the baseline
	// still lands, fenced only on its pending input state.
	require.NoError(t, reconcilePrimaryContractBaseline(ctx, f.options, "fixture"))
	require.True(t, f.state(t).Accepted)
	return f
}

func (f *contractColdDoorFixture) newMultiIndexer(t *testing.T) *MultiIndexer {
	t.Helper()
	manager, err := config.NewConfigManager(f.configPath)
	require.NoError(t, err)
	mi := NewMultiIndexer(f.store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	mi.SetContractCoreRuntime(f.hooks())
	return mi
}

func (f *contractColdDoorFixture) state(t *testing.T) graph.ContractInputState {
	t.Helper()
	state, found, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
	require.NoError(t, err)
	require.True(t, found)
	return state
}

func (f *contractColdDoorFixture) receipt(t *testing.T, path string) *graph.ContractBoundaryReceipt {
	t.Helper()
	row, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", path)
	require.NoError(t, err)
	require.NotNil(t, row, path)
	return row
}

// coldDoors are the cold entry points into generation zero. Each re-indexes
// the whole namespace of the already accepted fixture repository.
var coldDoors = []struct {
	name string
	run  func(t *testing.T, f *contractColdDoorFixture) *MultiIndexer
}{
	{"index_repo", func(t *testing.T, f *contractColdDoorFixture) *MultiIndexer {
		_, err := f.mi.IndexRepo("fixture")
		require.NoError(t, err)
		return f.mi
	}},
	{"index_multi_repo", func(t *testing.T, f *contractColdDoorFixture) *MultiIndexer {
		_, err := f.mi.IndexAll()
		require.NoError(t, err)
		return f.mi
	}},
	{"index_ctx", func(t *testing.T, f *contractColdDoorFixture) *MultiIndexer {
		_, err := f.mi.indexers["fixture"].IndexCtx(t.Context(), f.root)
		require.NoError(t, err)
		return f.mi
	}},
	{"reconcile_fallback", func(t *testing.T, f *contractColdDoorFixture) *MultiIndexer {
		t.Setenv("GORTEX_WARMUP_FULL_RETRACK", "1")
		restarted := f.newMultiIndexer(t)
		prior := map[string]int64{"routes.go": 1, "failing.go": 1}
		result, err := restarted.ReconcileRepoCtx(t.Context(), config.RepoEntry{Name: "fixture", Path: f.root}, prior)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.True(t, result.FullRetrack, "the forced reconcile takes the whole-repo retrack door")
		return restarted
	}},
}

// TestContractCoreColdDoorMatrix pins the journal acceptance at every cold
// door: the door accepts the journal it began (the namespace header returns
// to accepted), drains it, announces it once, and a later route edit still
// moves the input identity.
// The fault rows inject a refused begin for one file during the door: that
// file becomes an accepted failure envelope the census excludes; when even
// the envelope cannot be staged the header stays pending for the baseline.
// The in-flight row captures the next job while a save of a file the door
// accepted holds its core-write window open: the job waits for it and the
// publish equals a cold build.
func TestContractCoreColdDoorMatrix(t *testing.T) {
	// A row's oracle is a cold build of files, never of a door: the matrix
	// builds each distinct file set once.
	oracles := contractColdOracles{}
	coldIDs := func(t *testing.T, files map[string]string) []string {
		t.Helper()
		return contractIDs(oracles.contracts(t, files))
	}
	for _, door := range coldDoors {
		for _, fault := range []string{"none", "failed_file", "envelope_refused", "in_flight"} {
			t.Run(door.name+"/"+fault, func(t *testing.T) {
				f := newContractColdDoorFixture(t)
				before := f.state(t)
				failing := "fixture/failing.go"
				switch fault {
				case "failed_file":
					var once atomic.Bool
					refuse := func(path string) error {
						if path == failing && once.CompareAndSwap(false, true) {
							return errors.New("injected cold receipt flush failure")
						}
						return nil
					}
					contractCoreBeginFault.Store(&refuse)
				case "envelope_refused":
					refuse := func(path string) error {
						if path == failing {
							return errors.New("injected persistent receipt write failure")
						}
						return nil
					}
					contractCoreBeginFault.Store(&refuse)
				}
				publishedBefore := f.publishedFor("fixture")
				mi := door.run(t, f)
				contractCoreBeginFault.Store(nil)
				idx := mi.indexers["fixture"]
				require.NotNil(t, idx)
				require.Nil(t, idx.contractCoreInputs, "the door drains the journal it began")
				require.GreaterOrEqual(t, f.publishedFor("fixture")-publishedBefore, int64(1), "the cold door announces its acceptance")
				require.True(t, f.receipt(t, "fixture/routes.go").Accepted)
				after := f.state(t)
				switch fault {
				case "envelope_refused":
					require.False(t, after.Accepted, "a failed file the door could not exclude leaves the header pending")
					// The baseline then certifies the namespace from what the
					// core holds, and the next publish equals a cold build of
					// the fixture's files the core still holds, read from the
					// fixture and the core, not from the job's census: a door
					// that reset the namespace dropped the failed file's core,
					// one that re-indexes in place kept its unchanged rows.
					options := f.options
					options.MultiIndexer = mi
					require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), options, "fixture"))
					require.True(t, f.state(t).Accepted, "the baseline certifies the pending namespace")
					report, err := runFixtureContractJob(t, options, contractFixtureJob{layer: 1})
					require.NoError(t, err)
					require.True(t, report.Published)
					cold := make(map[string]string)
					for name, text := range map[string]string{"routes.go": coldDoorRoutes, "failing.go": coldDoorFailing} {
						held := false
						for _, node := range f.store.GetFileNodes("fixture/" + name) {
							held = held || node.Kind == graph.KindFile
						}
						if held {
							cold[name] = text
						}
					}
					require.Contains(t, cold, "routes.go")
					// IndexCtx re-indexes the namespace in place; every other
					// door resets it before its pass.
					inPlace := door.name == "index_ctx"
					require.Equal(t, inPlace, cold["failing.go"] != "", "%s: whether the core still holds the failed file", door.name)
					require.Equal(t, coldIDs(t, cold), contractIDs(publishedFixtureContracts(t, options)))
					return
				default:
					require.True(t, after.Accepted, "the cold door accepts its namespace")
				}
				require.NotEqual(t, before.InputFingerprint, after.InputFingerprint, "a cold namespace is a new input identity")
				envelope := f.receipt(t, failing)
				if fault == "failed_file" {
					require.True(t, envelope.Accepted)
					require.True(t, contractReceiptIsFailureEnvelope(envelope), "the failed file is an accepted failure envelope")
					inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
					require.NoError(t, err)
					options := f.options
					options.MultiIndexer = mi
					snapshot, _, err := NewContractFollowupCapture(options)(t.Context(), nil, inputs, "fixture", "")
					require.NoError(t, err)
					var paths []string
					for _, file := range snapshot.Files {
						paths = append(paths, file.Path)
					}
					snapshot.Release()
					// The cold pass may also have left the file without a census
					// row; either way it is not a live member the job reads.
					require.NotContains(t, paths, failing, "an envelope is not a live census member")
					require.Contains(t, paths, "fixture/routes.go")
					// The next publish equals a cold build of the census the
					// door accepted: the failed file's records are absent.
					report, err := runFixtureContractJob(t, options, contractFixtureJob{layer: 1})
					require.NoError(t, err)
					require.True(t, report.Published)
					require.Equal(t, coldIDs(t, map[string]string{"routes.go": coldDoorRoutes}), contractIDs(publishedFixtureContracts(t, options)))
				} else {
					require.False(t, contractReceiptIsFailureEnvelope(envelope))
				}
				if fault == "in_flight" {
					const routes = "fixture/routes.go"
					row := f.receipt(t, routes)
					staged := *row
					staged.Accepted, staged.Previous = false, nil
					require.NoError(t, f.store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{staged}))
					options := f.options
					options.MultiIndexer = mi
					closed := false
					report, err := runFixtureContractJob(t, options, contractFixtureJob{layer: 1, wait: contractInFlightWait(f.store, "fixture", routes, &closed),
						hooks: &contractConvergenceHooks{roundWait: 5 * time.Second}, yield: contractJobDeadline(time.Minute)})
					require.NoError(t, err)
					require.True(t, report.Published)
					require.True(t, closed, "the job waited for the in-flight window")
					require.Equal(t, coldIDs(t, map[string]string{"routes.go": coldDoorRoutes, "failing.go": coldDoorFailing}), contractIDs(publishedFixtureContracts(t, options)))
					return
				}
				// The accepted namespace still journals: a route edit moves the key.
				writeFile(t, filepath.Join(f.root, "routes.go"), "package fixture\nfunc register(r Router) { r.GET(\"/after\", serve) }\nfunc serve() {}\n")
				_, err := mi.IncrementalReindexRepo("fixture", []string{filepath.Join(f.root, "routes.go")})
				require.NoError(t, err)
				edited := f.state(t)
				require.True(t, edited.Accepted)
				require.NotEqual(t, after.InputFingerprint, edited.InputFingerprint, "a route edit after the door moves the input identity")
			})
		}
	}
}

// contractFailingExtractor is a test language whose extraction fails on
// demand: content with EXTRACT_FAIL makes the extractor return an error; with
// VANISH it also makes the file unreadable, so the incremental door's
// fallback re-read fails. The crash-isolation worker has no extractor for the
// language, so under isolation every extraction fails in the subprocess.
type contractFailingExtractor struct{ root string }

func (*contractFailingExtractor) Language() string     { return "boomlang" }
func (*contractFailingExtractor) Extensions() []string { return []string{".boom"} }
func (e *contractFailingExtractor) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	switch {
	case bytes.Contains(src, []byte("EXTRACT_FAIL")):
		return nil, errors.New("boomlang: unparseable source")
	case bytes.Contains(src, []byte("VANISH")):
		if err := os.Chmod(filepath.Join(e.root, filepath.FromSlash(path)), 0o000); err != nil {
			return nil, err
		}
		return nil, errors.New("boomlang: source vanished")
	}
	return &parser.ExtractionResult{Nodes: []*graph.Node{
		{ID: path, Name: filepath.Base(path), Kind: graph.KindFile, FilePath: path, Language: "boomlang"},
		{ID: path + "::Boom", Name: "Boom", Kind: graph.KindFunction, FilePath: path, StartLine: 1, EndLine: 1, Language: "boomlang"},
	}}, nil
}

// TestContractFailureEnvelopeEligibility pins the eligibility table. The
// production rows drive each site that marks a content failure through the
// real incremental door (the probe's read, indexFile's read on the fallback,
// an in-process extractor error, a crash-isolation worker error): the failed
// file must end as an accepted failure envelope that a job excludes, so
// removing any marker turns its row red. The classifier rows pin the table
// itself: a cold door excludes every failed file whatever the failure;
// elsewhere only a deterministic content failure becomes an envelope.
func TestContractFailureEnvelopeEligibility(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root process reads a mode-000 file")
	}
	for _, row := range []struct {
		name, content string
		unreadable    bool
		isolation     bool
	}{
		{name: "probe_read_error", content: "fine\n// unreadable\n", unreadable: true},
		{name: "index_file_read_error", content: "VANISH\n"},
		{name: "extractor_error", content: "EXTRACT_FAIL\n"},
		{name: "subprocess_error", content: "fine\n// parsed in the worker\n", isolation: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			extractor := &contractFailingExtractor{}
			registry := newTestRegistry()
			registry.Register(extractor)
			f := newContractJobFixtureWithRegistry(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "x.boom": "fine\n"}, registry)
			extractor.root = f.root
			before, found, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
			require.NoError(t, err)
			require.True(t, found)
			path := filepath.Join(f.root, "x.boom")
			t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
			if row.isolation {
				t.Setenv("GORTEX_PARSER_ISOLATION", "1")
				t.Setenv(crashWorkerEnv, "1")
			}
			writeFile(t, path, row.content)
			future := time.Now().Add(time.Hour)
			require.NoError(t, os.Chtimes(path, future, future))
			if row.unreadable {
				// Stale first, unreadable second: on Windows the read is
				// denied by a lock whose open handle refuses a later Chtimes.
				denyFileRead(t, path)
			}
			_, _ = f.mi.IncrementalReindexRepo("fixture", []string{path})
			receipt, _, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/x.boom")
			require.NoError(t, err)
			require.NotNil(t, receipt)
			require.True(t, receipt.Accepted)
			require.True(t, contractReceiptIsFailureEnvelope(receipt), "the failed file is an accepted failure envelope")
			after, _, err := f.store.ContractInputStateContext(t.Context(), "fixture", "")
			require.NoError(t, err)
			require.True(t, after.Accepted)
			require.NotEqual(t, before.InputFingerprint, after.InputFingerprint, "breaking the file moves the key once")
			report, err := f.job(nil, nil)
			require.NoError(t, err)
			require.True(t, report.Published, "the envelope is excluded, not a refused census member")
		})
	}
	rows := []struct {
		name        string
		err         error
		incremental bool
	}{
		{"read_permission", contractContentFailure(&os.PathError{Op: "open", Path: "x.go", Err: os.ErrPermission}), true},
		{"parse_failure", contractContentFailure(errors.New("tree-sitter: parse failed")), true},
		{"silent_zero", errSilentZeroExtraction, true},
		{"unrelated_version_race", errFileVersionChanged, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"journal_write_refused", errors.Join(errContractJournalWrite, errors.New("busy")), false},
		{"vanished_file", contractContentFailure(&os.PathError{Op: "open", Path: "x.go", Err: os.ErrNotExist}), false},
		// Failures that are not about the file's own bytes.
		{"journal_prepare_read", errors.New("read prior boundary receipt: database is locked"), false},
		{"parse_admission", errors.New("native parse admission: queue full"), false},
		{"unmarked_path_error", &os.PathError{Op: "lstat", Path: "x.go", Err: os.ErrPermission}, false},
	}
	for _, row := range rows {
		t.Run("classifier/"+row.name, func(t *testing.T) {
			require.Equal(t, row.incremental, contractFailureEnvelopeEligible(row.err, false), "incremental column")
			require.True(t, contractFailureEnvelopeEligible(row.err, true), "a cold door excludes every failed file")
		})
	}
	require.False(t, contractFailureEnvelopeEligible(nil, false))
}

// TestContractFailureEnvelopeMovesTheKeyOncePerTransition: breaking a file and
// fixing it each move the input identity exactly once; a version race and a
// still-failing file move none. The breaking save stages
// the envelope with no core write (its read failed), and the watcher door
// still accepts the pass.
func TestContractFailureEnvelopeMovesTheKeyOncePerTransition(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a root process reads a mode-000 file")
	}
	f := newContractColdDoorFixture(t)
	failing := filepath.Join(f.root, "failing.go")
	start := f.state(t)
	require.True(t, start.Accepted)

	// (b) break: an unreadable file is a deterministic content failure.
	touch := func(at time.Time) {
		require.NoError(t, os.Chtimes(failing, at, at))
	}
	// Stale first, unreadable second: on Windows the read is denied by a lock
	// whose open handle refuses a Chtimes, so every touch below lifts it.
	writeFile(t, failing, coldDoorFailing+"// a save that will not be readable\n")
	touch(time.Now().Add(time.Minute))
	allowRead := denyFileRead(t, failing)
	_, _ = f.mi.IncrementalReindexRepo("fixture", []string{failing})
	broken := f.state(t)
	require.True(t, broken.Accepted)
	require.NotEqual(t, start.InputFingerprint, broken.InputFingerprint, "breaking a file moves the key")
	row := f.receipt(t, "fixture/failing.go")
	require.True(t, row.Accepted)
	require.True(t, contractReceiptIsFailureEnvelope(row), "the unreadable file is an envelope")

	// (e) a second failed save of the same file restates no new cause.
	allowRead()
	touch(time.Now().Add(2 * time.Minute))
	allowRead = denyFileRead(t, failing)
	_, _ = f.mi.IncrementalReindexRepo("fixture", []string{failing})
	require.Equal(t, broken, f.state(t), "a still-failing file moves no key")

	// (d) a version race stages nothing.
	idx := f.mi.indexers["fixture"]
	backend, err := newPrimaryContractCoreStorageBackend(t.Context(), f.store, "fixture")
	require.NoError(t, err)
	idx.contractCoreInputs, err = newContractCoreInputJournal(t.Context(), backend)
	require.NoError(t, err)
	idx.stageContractFailureEnvelope(filepath.Join(f.root, "routes.go"), errFileVersionChanged)
	require.Empty(t, idx.contractCoreInputs.begun, "a version race is a retry signal, not content")
	idx.contractCoreInputs = nil
	require.Equal(t, broken, f.state(t))

	// (c) fix: the complete receipt replaces the envelope, one more move.
	allowRead()
	touch(time.Now().Add(3 * time.Minute))
	_, err = f.mi.IncrementalReindexRepo("fixture", []string{failing})
	require.NoError(t, err)
	fixed := f.state(t)
	require.True(t, fixed.Accepted)
	require.NotEqual(t, broken.InputFingerprint, fixed.InputFingerprint, "fixing the file moves the key")
	require.False(t, contractReceiptIsFailureEnvelope(f.receipt(t, "fixture/failing.go")))
}

// TestContractSilentZeroIsAnEnvelopeDecidedInPrepare pins the silent-zero row:
// prepare stages the failure envelope instead of a complete receipt, and a
// prior envelope makes a second silent-zero save cause-free.
func TestContractSilentZeroIsAnEnvelopeDecidedInPrepare(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	partial := &graph.Node{ID: "fixture/broken.go", Kind: graph.KindFile, FilePath: "fixture/broken.go", Meta: map[string]any{"parse_health": "partial"}}
	result := &parser.ExtractionResult{Nodes: []*graph.Node{partial}}
	require.True(t, silentZeroExtraction(result))
	backend := &contractCoreBackendProbe{known: true}
	journal, err := newContractCoreInputJournal(t.Context(), backend)
	require.NoError(t, err)
	require.NoError(t, journal.prepare(t.Context(), idx, "fixture/broken.go", "go", []byte("package \x00"), result, false))
	change := journal.changes["fixture/broken.go"]
	require.Nil(t, change.Current, "no complete receipt is certified for a silent-zero parse")
	require.Equal(t, contractFileIndexFailed, change.Uncertainty)
	require.True(t, change.Delta.Scope.Unknown)
	require.Contains(t, change.Delta.Scope.Causes, contractFileIndexFailed)
}

// TestContractBaselineLandsUnderUnrelatedSavesAndYieldsToBoundaryEdits: a
// body-only save inside the baseline's chunk does not refuse it (the
// rewritten row is skipped and kept), while a key-moving save supersedes it
// and leaves the namespace pending for a re-admission.
func TestContractBaselineLandsUnderUnrelatedSavesAndYieldsToBoundaryEdits(t *testing.T) {
	for _, mode := range []string{"body_save", "boundary_save"} {
		t.Run(mode, func(t *testing.T) {
			f := newContractColdDoorFixture(t)
			// Reopen a pending namespace for a fresh baseline.
			_, err := f.mi.IndexRepo("fixture")
			require.NoError(t, err)
			state := f.state(t)
			current := state
			current.Accepted = false
			next := current
			next.InputFingerprint = contractInputHash([]byte("reopened-for-baseline"))
			require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &state, next, nil))
			saved := false
			options := f.options
			options.Yield = func(ctx context.Context) error {
				if !saved {
					saved = true
					source := coldDoorFailing + "func bodyOnly() int { return 2 }\n"
					if mode == "boundary_save" {
						source = "package fixture\nfunc registerFailing(r Router) { r.GET(\"/moved\", failing) }\nfunc failing() {}\n"
					}
					writeFile(t, filepath.Join(f.root, "failing.go"), source)
					if _, err := f.mi.IncrementalReindexRepo("fixture", []string{filepath.Join(f.root, "failing.go")}); err != nil {
						return err
					}
				}
				return ctx.Err()
			}
			err = reconcilePrimaryContractBaseline(t.Context(), options, "fixture")
			require.True(t, saved)
			baseline, baselineErr := f.store.ContractBoundaryReceiptBaselineContext(t.Context(), "fixture", "")
			require.NoError(t, baselineErr)
			if mode == "boundary_save" {
				require.ErrorIs(t, err, graph.ErrContractProjectionStale, "a key-moving save supersedes the baseline")
				return
			}
			require.NoError(t, err, "an unrelated body-only save does not refuse the baseline")
			require.NotNil(t, baseline)
			require.True(t, f.state(t).Accepted)
			require.True(t, f.receipt(t, "fixture/failing.go").Accepted)
		})
	}
}

// TestContractBaselinePanicReleasesItsSlotAndWaiters pins recover in the
// baseline job: a panicking reconstruction still releases its admission slot
// and wakes waiters, and the next admission is served.
func TestContractBaselinePanicReleasesItsSlotAndWaiters(t *testing.T) {
	f := newContractColdDoorFixture(t)
	_, err := f.mi.IndexRepo("fixture")
	require.NoError(t, err)
	state := f.state(t)
	next := state
	next.Accepted = false
	next.InputFingerprint = contractInputHash([]byte("reopened-for-panic"))
	require.NoError(t, f.store.BeginContractInputMutationContext(t.Context(), &state, next, nil))
	require.NoError(t, f.store.SetContractBoundaryReceiptBaselineContext(t.Context(), graph.ContractBoundaryReceiptBaseline{RepoPrefix: "fixture", Version: "outdated", Fingerprint: "outdated"}))
	var admission sync.Mutex
	var jobs sync.WaitGroup
	options := f.options
	options.BaselineAdmissionMu, options.BaselineJobs = &admission, &jobs
	panicked := atomic.Bool{}
	options.Yield = func(ctx context.Context) error {
		if panicked.CompareAndSwap(false, true) {
			panic("baseline job panic")
		}
		return ctx.Err()
	}
	before := f.publishedFor("fixture")
	reconcile := NewContractBaselineReconciler(options)
	admitted, err := reconcile(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	require.True(t, admitted)
	done := make(chan struct{})
	go func() { jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a panicking baseline job never released its slot")
	}
	require.True(t, panicked.Load())
	require.Greater(t, f.publishedFor("fixture"), before, "waiters are woken after a panic")
	admitted, err = reconcile(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	require.True(t, admitted, "the released slot admits the next baseline")
	jobs.Wait()
	require.True(t, f.state(t).Accepted)
}
