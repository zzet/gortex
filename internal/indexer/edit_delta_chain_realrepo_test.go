package indexer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
)

// TestRealRepoChainedEditDelta measures consecutive edits of different files
// in process, each chained over the generation the previous one published, as
// an agent editing file after file does: the first edit is built direct over
// the checkout's base (a fixed commit stack key, as a routed commit layer
// has), and every later one over the chain. GX_DELTA_CHAIN_MODE=before builds
// with the chain in the per-stack keys (every chained edit a new key, the
// keying before the overlay); after (the default) keys them by the commit
// stack and composes the chain per read. Opt-in: GX_DELTA_WARM_TREE and
// GX_DELTA_WARM_STORE as TestRealRepoWarmEditDelta; GX_DELTA_CHAIN_FILES (the
// files edited in order, one per edit), GX_DELTA_CHAIN_ROUNDS (default 3, a
// fresh store copy each), GX_DELTA_CHAIN_OUT (a JSON file of the records).
func TestRealRepoChainedEditDelta(t *testing.T) {
	tree := os.Getenv("GX_DELTA_WARM_TREE")
	storePath := os.Getenv("GX_DELTA_WARM_STORE")
	if tree == "" || storePath == "" {
		t.Skip("GX_DELTA_WARM_TREE / GX_DELTA_WARM_STORE are not set")
	}
	builderIsolateGit(t)
	if out := builderGit(t, tree, "status", "--porcelain"); out != "" {
		t.Fatalf("the clone at %s is not clean:\n%s", tree, out)
	}
	cfg, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's configuration: %v", err)
	}
	mode := os.Getenv("GX_DELTA_CHAIN_MODE")
	if mode == "" {
		mode = "after"
	}
	if mode != "after" && mode != "before" {
		t.Fatalf("GX_DELTA_CHAIN_MODE=%q, want before or after", mode)
	}
	if mode == "before" {
		editDeltaChainOverlay = false
		t.Cleanup(func() { editDeltaChainOverlay = true })
	}
	files := editDeltaChainDefaultFiles
	if raw := os.Getenv("GX_DELTA_CHAIN_FILES"); raw != "" {
		files = strings.Split(raw, ",")
	}
	rounds := 3
	if n, err := strconv.Atoi(os.Getenv("GX_DELTA_CHAIN_ROUNDS")); err == nil && n > 0 {
		rounds = n
	}
	// GX_DELTA_CHAIN_ROTATE=1 rotates the file order by one per round, so
	// over len(files) rounds every file is edited at every depth.
	rotate := os.Getenv("GX_DELTA_CHAIN_ROTATE") == "1"
	var records []editDeltaChainRecord
	for round := 1; round <= rounds; round++ {
		order := files
		if rotate {
			shift := (round - 1) % len(files)
			order = append(append([]string(nil), files[shift:]...), files[:shift]...)
		}
		records = append(records, editDeltaChainRound(t, tree, storePath, order, cfg.Index, mode, round)...)
	}
	if out := os.Getenv("GX_DELTA_CHAIN_OUT"); out != "" {
		if data, err := json.MarshalIndent(records, "", "  "); err == nil {
			_ = os.WriteFile(out, data, 0o644)
		}
	}
}

var editDeltaChainDefaultFiles = []string{
	"internal/config/config.go",
	"internal/gitstate/dirty.go",
	"internal/indexer/checkout_refresh.go",
	"internal/mcp/checkout_binding.go",
	"internal/graph/overlay.go",
	"internal/resolver/resolver.go",
	"internal/indexer/indexer.go",
	"internal/graphview/materialize.go",
	"internal/graph/store_sqlite/store.go",
}

// editDeltaChainRecord is one chained edit's measurement.
type editDeltaChainRecord struct {
	Mode   string             `json:"mode"`
	Round  int                `json:"round"`
	File   string             `json:"file"`
	Depth  int                `json:"chain_depth"` // chain layers the edit stands on
	WallMS float64            `json:"wall_ms"`
	CPUMS  float64            `json:"cpu_ms"`
	Phases map[string]float64 `json:"phases_ms"`
	CPU    map[string]float64 `json:"phase_cpu_ms"`
	Build  map[string]float64 `json:"build_phases_ms"`
	// Overlaid is the chain layers the delta composed over the caches.
	Overlaid int `json:"chain_layers_overlaid"`
	// RegistryCached reports the contract registry came from the cache.
	RegistryCached bool `json:"contract_registry_cached"`
	// AffectedKeys / AffectedFiles / AffectedSample: the affected-by plan's
	// changed declarations and the referrer files it re-resolved.
	AffectedKeys   int      `json:"affected_by_keys"`
	AffectedFiles  int      `json:"affected_by_files"`
	AffectedSample []string `json:"affected_by_key_sample"`
	// Load is the host's one-minute load average when the edit ended.
	Load float64 `json:"load1"`
}

// editDeltaChainCommit is the fake commit generation the base store stands
// for: generation zero is the one mutable generation, and a real one would
// not be keyed.
const editDeltaChainCommit = int64(1) << 40

func editDeltaChainRound(t *testing.T, tree, storePath string, files []string, cfg config.IndexConfig, mode string, round int) []editDeltaChainRecord {
	t.Helper()
	resetChainOverlayCaches()
	copyPath := filepath.Join(t.TempDir(), "store.sqlite")
	if out, err := exec.Command("cp", "-c", storePath, copyPath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	if file, err := os.Open(copyPath); err == nil {
		_, _ = io.Copy(io.Discard, file)
		_ = file.Close()
	}
	store := builderOpenStoreAt(t, copyPath)
	defer func() { _ = store.Close() }()
	builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: cfg, Logger: editDeltaRealLogger(t)}
	sampler, err := gitstate.NewDirtySampler(tree, "", "")
	if err != nil {
		t.Fatalf("open the checkout's sampler: %v", err)
	}
	originals := make(map[string][]byte, len(files))
	defer func() {
		for rel, body := range originals {
			_ = os.WriteFile(filepath.Join(tree, filepath.FromSlash(rel)), body, 0o644)
		}
	}()
	ctx := context.Background()
	var chain []int64
	var records []editDeltaChainRecord
	for i, rel := range files {
		rel = strings.TrimSpace(rel)
		full := filepath.Join(tree, filepath.FromSlash(rel))
		original, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		originals[rel] = original
		edited, ok := editDeltaRealBodyEdit(string(original), 1+round)
		if !ok {
			t.Fatalf("%s has no function body to edit", rel)
		}
		if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		req := DirtyLayerRequest{
			Identity:     builderDirtyIdentity(),
			Base:         commitLayerBase{Reader: store, corpus: store, stack: []int64{editDeltaChainCommit}},
			CheckoutRoot: tree, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
			Sampler: sampler,
		}
		if len(chain) > 0 {
			manifest, why := loadDirtyChainManifest(ctx, store, chain)
			if why != "" {
				t.Fatalf("edit %d (%s): the chain's manifest: %s", i, rel, why)
			}
			parent := chain[len(chain)-1]
			req.Base = commitLayerBase{
				Reader: dirtyChainComposed(t, store, chain), corpus: store,
				stack: append([]int64{editDeltaChainCommit}, chain...),
			}
			req.Identity.BaseGenerationID = parent
			req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(chain)
		}
		recordLastEditDelta(nil)
		started, cpuStarted := time.Now(), editDeltaProcessCPU()
		id, report, err := builder.BuildDirtyLayer(ctx, req)
		if err != nil {
			t.Fatalf("edit %d (%s) over a chain of %d: %v", i, rel, len(chain), err)
		}
		if len(chain) > 0 && report.ParentGenerationID != chain[len(chain)-1] {
			t.Fatalf("edit %d (%s) was not built over the chain (parent %d, fallback %q)", i, rel, report.ParentGenerationID, report.ChainFallbackReason)
		}
		rec := editDeltaChainRecord{Mode: mode, Round: round, File: rel, Depth: len(chain),
			WallMS: ms(time.Since(started)), CPUMS: ms(editDeltaProcessCPU() - cpuStarted),
			Phases: map[string]float64{}, CPU: map[string]float64{}, Build: map[string]float64{}}
		if d := LastEditDeltaReport(); d != nil {
			for _, p := range d.Phases {
				rec.Phases[p.Name] += ms(p.Duration)
			}
			for name, c := range d.PhaseCPU {
				rec.CPU[name] = ms(c)
			}
			rec.Overlaid = d.ChainLayersOverlaid
			rec.RegistryCached = d.ContractRegistryCached
			rec.AffectedKeys, rec.AffectedFiles, rec.AffectedSample = d.AffectedByKeys, d.AffectedByFiles, d.AffectedByKeySample
		}
		if report.Work != nil {
			for _, p := range report.Work.Phases {
				rec.Build[p.Name] += ms(p.Duration)
			}
		}
		rec.Load = editDeltaLoadAverage()
		records = append(records, rec)
		t.Logf("%s round %d depth %d %s: wall=%.1fms cpu=%.1fms overlaid=%d affected=%d keys/%d files load=%.2f %s", mode, round, rec.Depth, rel,
			rec.WallMS, rec.CPUMS, rec.Overlaid, rec.AffectedKeys, rec.AffectedFiles, rec.Load, editDeltaWarmLine(rec.Phases))
		chain = append(chain, id)
	}
	return records
}

// TestRealRepoChainOverlayReadCost measures what composing a dirty chain over
// the per-stack caches costs per read: each read a delta serves from the
// caches, asked with a package's paths, identities and names, over the commit
// stack alone and over chains of 1, 4, 8 and 16 real edit layers, with the
// caches warm. Opt-in as TestRealRepoChainedEditDelta; GX_DELTA_OVERLAY_DEPTHS
// (default 1,4,8,16), GX_DELTA_OVERLAY_PACKAGE (default internal/indexer),
// GX_DELTA_OVERLAY_REPS (default 20), GX_DELTA_OVERLAY_OUT.
func TestRealRepoChainOverlayReadCost(t *testing.T) {
	tree := os.Getenv("GX_DELTA_WARM_TREE")
	storePath := os.Getenv("GX_DELTA_WARM_STORE")
	if tree == "" || storePath == "" {
		t.Skip("GX_DELTA_WARM_TREE / GX_DELTA_WARM_STORE are not set")
	}
	builderIsolateGit(t)
	if out := builderGit(t, tree, "status", "--porcelain"); out != "" {
		t.Fatalf("the clone at %s is not clean:\n%s", tree, out)
	}
	cfg, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's configuration: %v", err)
	}
	depths := []int{1, 4, 8, 16}
	if raw := os.Getenv("GX_DELTA_OVERLAY_DEPTHS"); raw != "" {
		depths = nil
		for _, part := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 {
				t.Fatalf("GX_DELTA_OVERLAY_DEPTHS entry %q", part)
			}
			depths = append(depths, n)
		}
	}
	pkg := os.Getenv("GX_DELTA_OVERLAY_PACKAGE")
	if pkg == "" {
		pkg = "internal/indexer"
	}
	reps := 20
	if n, err := strconv.Atoi(os.Getenv("GX_DELTA_OVERLAY_REPS")); err == nil && n > 0 {
		reps = n
	}
	maxDepth := depths[len(depths)-1]
	// Build the chain: one body edit per file, each chained over the last,
	// files taken round-robin from the edited list so a file is edited again
	// at a later depth too.
	resetChainOverlayCaches()
	copyPath := filepath.Join(t.TempDir(), "store.sqlite")
	if out, err := exec.Command("cp", "-c", storePath, copyPath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	store := builderOpenStoreAt(t, copyPath)
	t.Cleanup(func() { _ = store.Close() })
	builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: cfg.Index, Logger: editDeltaRealLogger(t)}
	sampler, err := gitstate.NewDirtySampler(tree, "", "")
	if err != nil {
		t.Fatalf("open the checkout's sampler: %v", err)
	}
	ctx := context.Background()
	originals := map[string][]byte{}
	t.Cleanup(func() {
		for rel, body := range originals {
			_ = os.WriteFile(filepath.Join(tree, filepath.FromSlash(rel)), body, 0o644)
		}
	})
	var chain []int64
	for i := 0; i <= maxDepth; i++ {
		rel := editDeltaChainDefaultFiles[i%len(editDeltaChainDefaultFiles)]
		full := filepath.Join(tree, filepath.FromSlash(rel))
		if _, ok := originals[rel]; !ok {
			body, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			originals[rel] = body
		}
		edited, _ := editDeltaRealBodyEdit(string(originals[rel]), i+1)
		if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		req := DirtyLayerRequest{
			Identity:     builderDirtyIdentity(),
			Base:         commitLayerBase{Reader: store, corpus: store, stack: []int64{editDeltaChainCommit}},
			CheckoutRoot: tree, RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
			Sampler: sampler,
		}
		if len(chain) > 0 {
			manifest, why := loadDirtyChainManifest(ctx, store, chain)
			if why != "" {
				t.Fatalf("the chain's manifest: %s", why)
			}
			parent := chain[len(chain)-1]
			req.Base = commitLayerBase{Reader: dirtyChainComposed(t, store, chain), corpus: store,
				stack: append([]int64{editDeltaChainCommit}, chain...)}
			req.Identity.BaseGenerationID = parent
			req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(chain)
		}
		id, _, err := builder.BuildDirtyLayer(ctx, req)
		if err != nil {
			t.Fatalf("chain edit %d: %v", i, err)
		}
		chain = append(chain, id)
	}
	// The inputs: a package's files as the base holds them.
	var paths []string
	for n := range store.NodesByKind(graph.KindFile) {
		if n != nil && strings.HasPrefix(n.FilePath, builderRepoPrefix+"/"+pkg+"/") && !strings.Contains(strings.TrimPrefix(n.FilePath, builderRepoPrefix+"/"+pkg+"/"), "/") {
			paths = append(paths, n.FilePath)
		}
	}
	sort.Strings(paths)
	var ids, names []string
	seenName := map[string]struct{}{}
	for _, nodes := range store.GetFileNodesByPaths(paths) {
		for _, n := range nodes {
			ids = append(ids, n.ID)
			if _, dup := seenName[n.Name]; !dup && n.Name != "" {
				seenName[n.Name] = struct{}{}
				names = append(names, n.Name)
			}
		}
	}
	sort.Strings(ids)
	sort.Strings(names)
	t.Logf("inputs: %d paths, %d identities, %d names; chain %v", len(paths), len(ids), len(names), chain)

	type readCost struct {
		Read    string  `json:"read"`
		Depth   int     `json:"depth"`
		PerCall float64 `json:"us_per_call"`
	}
	var costs []readCost
	measure := func(depth int) {
		base := commitLayerBase{Reader: store, corpus: store, stack: []int64{editDeltaChainCommit}}
		if depth > 0 {
			base = commitLayerBase{Reader: dirtyChainComposed(t, store, chain[:depth]), corpus: store,
				stack: append([]int64{editDeltaChainCommit}, chain[:depth]...), chainDepth: depth}
		}
		reads := map[string]func(dw *graph.DeltaWriter){
			"file_nodes": func(dw *graph.DeltaWriter) { dw.GetFileNodesByPaths(paths) },
			"nodes_by_kind": func(dw *graph.DeltaWriter) {
				dw.NodesInFilesByKind(paths, []graph.NodeKind{graph.KindType, graph.KindInterface})
			},
			"file_identities": func(dw *graph.DeltaWriter) {
				for range dw.FileNodeIdentitiesSeq([]string{builderRepoPrefix}) {
				}
			},
			"repo_names":       func(dw *graph.DeltaWriter) { dw.FindNodesByNamesInRepo(names, builderRepoPrefix) },
			"placements":       func(dw *graph.DeltaWriter) { dw.NodePlacementsByIDs(ids) },
			"in_identities":    func(dw *graph.DeltaWriter) { dw.GetInEdgeIdentitiesByNodeIDs(ids) },
			"import_adjacency": func(dw *graph.DeltaWriter) { dw.ProjectImportAdjacency(paths) },
			"recorded_edges":   func(dw *graph.DeltaWriter) { r, _ := graph.RecordedEdgesOf(dw); r.RecordedEdgesAt(paths) },
			"chain_touched":    func(dw *graph.DeltaWriter) { dw.ChainTouchedPaths() },
			"prior_edges_overlay": func(dw *graph.DeltaWriter) {
				in, _ := dw.EdgesBelowChainByNodeIDs(ids, true)
				dw.OverlayEdgesAboveChain(ids, in, true)
			},
		}
		readNames := make([]string, 0, len(reads))
		for name := range reads {
			readNames = append(readNames, name)
		}
		sort.Strings(readNames)
		for _, name := range readNames {
			read := reads[name]
			// Warm: one delta over the depth fills the caches for the inputs.
			warm := graph.NewDeltaWriter(base, nil)
			installEditDeltaStackCache(warm, base, store)
			read(warm)
			started := time.Now()
			for r := 0; r < reps; r++ {
				dw := graph.NewDeltaWriter(base, nil)
				installEditDeltaStackCache(dw, base, store)
				read(dw)
			}
			per := float64(time.Since(started).Microseconds()) / float64(reps)
			costs = append(costs, readCost{Read: name, Depth: depth, PerCall: per})
			t.Logf("depth %2d %-20s %9.1f us/call", depth, name, per)
		}
	}
	measure(0)
	for _, d := range depths {
		measure(d)
	}
	if out := os.Getenv("GX_DELTA_OVERLAY_OUT"); out != "" {
		if data, err := json.MarshalIndent(costs, "", "  "); err == nil {
			_ = os.WriteFile(out, data, 0o644)
		}
	}
}

// editDeltaLoadAverage is the host's one-minute load average (0 when unknown).
func editDeltaLoadAverage() float64 {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}
