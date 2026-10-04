package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
)

// The real-repository resolver measurement. It is skipped unless
// GX_REALREPO_TREE names a private clone of this repository (a standalone git
// repository whose HEAD is the committed base) and GX_REALREPO_STORE_DIR names
// a private directory for the stores and logs. Nothing here reads or writes a
// user store: the base corpus is indexed once into <store dir>/base.sqlite and
// every experiment runs on an APFS clone of it that is removed afterwards.
//
// Per edited file it records, at GOMAXPROCS=1:
//
//   - sparse: one working-tree layer build (BuildDirtyLayer, direct over the
//     committed corpus) of a body edit — total wall time, the build's phases and
//     the index pass's resolver record ("indexer: resolve pass");
//   - incremental: the primary checkout's per-save path (IndexFile, whose
//     resolver runs ResolveFileAndIncoming) followed by the batched incremental
//     resolver (ResolveFilesAndIncoming, the path the daemon's deferred and
//     watcher-batch catch-up use) with every phase line captured.
//
// Knobs: GX_REALREPO_FILES (comma list of repository-relative paths),
// GX_REALREPO_MODES (comma list of sparse,incremental), GX_REALREPO_EDIT
// (body|signature|import; default body), GX_REALREPO_OUT (a JSON artifact).

type realRepoExperiment struct {
	File      string             `json:"file"`
	Mode      string             `json:"mode"`
	Edit      string             `json:"edit"`
	ElapsedMS float64            `json:"elapsed_ms"`
	StepsMS   map[string]float64 `json:"steps_ms,omitempty"`
	Phases    map[string]float64 `json:"phases_ms,omitempty"`
	ParserIn  int                `json:"parser_inputs,omitempty"`
	PlanSize  int                `json:"plan_indexed,omitempty"`
	LogPath   string             `json:"log"`
	// SurfaceDiff lists, for the incremental mode, every declaration whose
	// fields differ across the edit (field names only, positions excluded).
	SurfaceDiff []string `json:"surface_diff,omitempty"`
	// Error records a failed arm that did not abort the run.
	Error string `json:"error,omitempty"`
	// Identity is the durable-row digest of an identity arm.
	Identity   map[string]string `json:"identity,omitempty"`
	ResolverLn []json.RawMessage `json:"resolver_lines,omitempty"`
}

func TestRealRepoResolverScope(t *testing.T) {
	tree := os.Getenv("GX_REALREPO_TREE")
	storeDir := os.Getenv("GX_REALREPO_STORE_DIR")
	if tree == "" || storeDir == "" {
		t.Skip("set GX_REALREPO_TREE and GX_REALREPO_STORE_DIR to a private clone and store directory")
	}
	builderIsolateGit(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	cfg, err := config.Load(filepath.Join(tree, ".gortex.yaml"))
	if err != nil {
		t.Fatalf("load the clone's config: %v", err)
	}
	indexCfg := cfg.Index

	base := filepath.Join(storeDir, "base.sqlite")
	if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
		started := time.Now()
		store := builderOpenStoreAt(t, base+".tmp")
		logger := realRepoLogger(t, filepath.Join(storeDir, "base-index.log"))
		idx := New(store, builderRegistry(), indexCfg, logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		if _, err := idx.Index(tree); err != nil {
			t.Fatalf("index the base corpus: %v", err)
		}
		idx.Close()
		if err := store.Close(); err != nil {
			t.Fatalf("close the base store: %v", err)
		}
		_ = logger.Sync()
		if err := os.Rename(base+".tmp", base); err != nil {
			t.Fatalf("publish the base store: %v", err)
		}
		t.Logf("indexed base corpus in %v", time.Since(started))
	}

	if os.Getenv("GX_REALREPO_BASE_ONLY") == "1" {
		// Only the base index: the digest of a whole index, e.g. to compare
		// two resolver configurations of the same tree.
		store, err := store_sqlite.Open(base)
		if err != nil {
			t.Fatalf("open the base store: %v", err)
		}
		digest := realRepoDurableDigest(t, store)
		_ = store.Close()
		if path := os.Getenv("GX_REALREPO_OUT"); path != "" {
			data, _ := json.MarshalIndent(digest, "", "  ")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
		}
		return
	}
	runtime.GOMAXPROCS(1)
	files := strings.Split(envOr("GX_REALREPO_FILES", "internal/config/config.go,internal/mcp/checkout_binding.go"), ",")
	modes := strings.Split(envOr("GX_REALREPO_MODES", "sparse,incremental"), ",")
	edit := envOr("GX_REALREPO_EDIT", "body")
	var out []realRepoExperiment
	for _, file := range files {
		for _, mode := range modes {
			out = append(out, runRealRepoExperiment(t, tree, storeDir, base, indexCfg, file, mode, edit))
		}
	}
	if path := os.Getenv("GX_REALREPO_OUT"); path != "" {
		data, _ := json.MarshalIndent(out, "", "  ")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func realRepoLogger(t testing.TB, path string) *zap.Logger {
	t.Helper()
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	cfg.OutputPaths = []string{path}
	cfg.ErrorOutputPaths = []string{path}
	cfg.Sampling = nil
	logger, err := cfg.Build()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	return logger
}

// realRepoEdit applies one edit to rel and returns the restore function.
//
//   - body: a statement inserted at the top of the first function body whose
//     signature line ends in "{" — no declaration, signature or import changes;
//   - signature: a new trailing exported function (a declaration added);
//   - import: a blank import of a standard-library package the file lacks.
func realRepoEdit(t testing.TB, tree, rel, kind string) func() {
	t.Helper()
	full := filepath.Join(tree, filepath.FromSlash(rel))
	original, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	src := string(original)
	var edited string
	switch kind {
	case "body":
		lines := strings.Split(src, "\n")
		done := false
		for i, line := range lines {
			if strings.HasPrefix(line, "func ") && strings.HasSuffix(strings.TrimSpace(line), "{") {
				lines = append(lines[:i+1], append([]string{"\t_ = len(\"resolver-scope-body-edit\")"}, lines[i+1:]...)...)
				done = true
				break
			}
		}
		if !done {
			t.Fatalf("no function body in %s", rel)
		}
		edited = strings.Join(lines, "\n")
	case "signature":
		edited = src + "\n// ResolverScopeProbe is a declaration added by the measurement.\nfunc ResolverScopeProbe() int { return 1 }\n"
	case "import":
		i := strings.Index(src, "\nimport (")
		if i < 0 {
			t.Fatalf("no import block in %s", rel)
		}
		i += len("\nimport (")
		edited = src[:i] + "\n\t_ \"hash/adler32\"" + src[i:]
	default:
		t.Fatalf("unknown edit kind %q", kind)
	}
	if err := os.WriteFile(full, []byte(edited), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	return func() {
		if err := os.WriteFile(full, original, 0o644); err != nil {
			t.Fatalf("restore %s: %v", rel, err)
		}
	}
}

func runRealRepoExperiment(
	t *testing.T, tree, storeDir, base string, indexCfg config.IndexConfig, rel, mode, edit string,
) realRepoExperiment {
	t.Helper()
	label := fmt.Sprintf("%s-%s-%s", strings.ReplaceAll(rel, "/", "_"), mode, edit)
	if mode == "clean" {
		return runRealRepoClean(t, tree, storeDir, indexCfg, rel, edit, label)
	}
	expPath := filepath.Join(storeDir, "exp.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(expPath + suffix)
	}
	if out, err := exec.Command("cp", "-c", base, expPath).CombinedOutput(); err != nil {
		t.Fatalf("clone the base store: %v\n%s", err, out)
	}
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(expPath + suffix)
		}
	}()
	store, err := store_sqlite.Open(expPath)
	if err != nil {
		t.Fatalf("open the experiment store: %v", err)
	}
	defer func() {
		_ = store.Close()
		// GX_REALREPO_KEEP=1 keeps the closed experiment store (an APFS
		// clone of the base: only the pages it wrote cost disk) for a
		// row-level comparison of two arms.
		if os.Getenv("GX_REALREPO_KEEP") == "1" {
			_ = os.Rename(expPath, filepath.Join(storeDir, "exp-"+label+".sqlite"))
		}
	}()
	logPath := filepath.Join(storeDir, label+".log")
	_ = os.Remove(logPath)
	logger := realRepoLogger(t, logPath)
	restore := realRepoEdit(t, tree, rel, edit)
	defer restore()

	rec := realRepoExperiment{File: rel, Mode: mode, Edit: edit, LogPath: logPath, StepsMS: map[string]float64{}}
	switch mode {
	case "sparse":
		builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: indexCfg, Logger: logger}
		started := time.Now()
		var report BuildReport
		var err error
		realRepoProfile(t, storeDir, label, "sparse_build", func() {
			_, report, err = builder.BuildDirtyLayer(context.Background(), DirtyLayerRequest{
				Identity: builderDirtyIdentity(), Base: store, CheckoutRoot: tree,
				RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
			})
		})
		rec.ElapsedMS = ms(time.Since(started))
		if err != nil {
			// The sparse arm measures code outside the resolver too; record its
			// failure and keep measuring the resolver arms.
			rec.Error = err.Error()
			t.Logf("%s: BuildDirtyLayer failed: %v", label, err)
		}
		if report.Work != nil {
			rec.Phases = map[string]float64{}
			for _, p := range report.Work.Phases {
				rec.Phases[p.Name] += ms(p.Duration)
			}
			rec.ParserIn, rec.PlanSize = report.Work.ParserInputs, report.Work.PlanIndexed
		}
	case "incremental":
		idx := New(store, builderRegistry(), indexCfg, logger)
		idx.resolver.SetLogger(logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		idx.rootPath = tree
		graphPath := idx.prefixPath(rel)
		before := store.GetFileNodes(graphPath)
		started := time.Now()
		realRepoProfile(t, storeDir, label, "index_file", func() {
			if err := idx.IndexFile(filepath.Join(tree, filepath.FromSlash(rel))); err != nil {
				t.Fatalf("%s: IndexFile: %v", label, err)
			}
		})
		rec.StepsMS["index_file"] = ms(time.Since(started))
		rec.SurfaceDiff = realRepoSurfaceDiff(before, store.GetFileNodes(graphPath))
		// The deferred/master catch-up shape: the same file frontier, first
		// without pre-mutation evidence (what the multi-repository catch-up
		// hands the resolver today), then with the declaration surface the
		// per-save path now carries.
		// GX_REALREPO_REPEAT repeats both catch-up shapes (samples for a
		// median); every repeat after the first is a warm re-resolve of the
		// same frontier.
		repeat := 1
		if n, err := strconv.Atoi(os.Getenv("GX_REALREPO_REPEAT")); err == nil && n > 1 {
			repeat = n
		}
		for i := 0; i < repeat; i++ {
			suffix := ""
			if i > 0 {
				suffix = fmt.Sprintf("_%d", i+1)
			}
			batch := time.Now()
			realRepoProfile(t, storeDir, label, "resolve_files"+suffix, func() {
				idx.resolver.ResolveFilesAndIncoming([]string{graphPath})
			})
			rec.StepsMS["resolve_files_and_incoming"+suffix] = ms(time.Since(batch))
			evidence := time.Now()
			idx.resolver.SetPriorDeclarations(map[string]resolver.DeclarationSurface{
				graphPath: resolver.DeclarationSurfaceOf(before),
			})
			scoping := idx.resolver.EvidenceScoping()
			idx.resolver.SetEvidenceScoping(true)
			realRepoProfile(t, storeDir, label, "resolve_files_with_evidence"+suffix, func() {
				idx.resolver.ResolveFilesAndIncoming([]string{graphPath})
			})
			idx.resolver.SetEvidenceScoping(scoping)
			idx.resolver.SetPriorDeclarations(nil)
			rec.StepsMS["resolve_files_and_incoming_with_evidence"+suffix] = ms(time.Since(evidence))
		}
		rec.ElapsedMS = ms(time.Since(started))
		idx.Close()
	case "identity-on", "identity-off":
		// One arm of the differential: the per-save path with evidence
		// scoping on or off (the pre-change behaviour), then a canonical
		// digest of every durable row resolution writes.
		if mode == "identity-off" {
			t.Setenv(resolver.EvidenceScopeEnv, "off")
		} else {
			t.Setenv(resolver.EvidenceScopeEnv, "on")
		}
		idx := New(store, builderRegistry(), indexCfg, logger)
		idx.resolver.SetLogger(logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		idx.rootPath = tree
		graphPath := idx.prefixPath(rel)
		before := store.GetFileNodes(graphPath)
		started := time.Now()
		if err := idx.IndexFile(filepath.Join(tree, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s: IndexFile: %v", label, err)
		}
		rec.StepsMS["index_file"] = ms(time.Since(started))
		master := time.Now()
		idx.resolver.SetPriorDeclarations(map[string]resolver.DeclarationSurface{
			graphPath: resolver.DeclarationSurfaceOf(before),
		})
		idx.resolver.ResolveFilesAndIncoming([]string{graphPath})
		idx.resolver.SetPriorDeclarations(nil)
		rec.StepsMS["resolve_files_and_incoming_with_evidence"] = ms(time.Since(master))
		rec.ElapsedMS = ms(time.Since(started))
		idx.Close()
		rec.SurfaceDiff = realRepoSurfaceDiff(before, store.GetFileNodes(graphPath))
		rec.Identity = realRepoDurableDigest(t, store)
	default:
		t.Fatalf("unknown mode %q", mode)
	}
	_ = logger.Sync()
	rec.ResolverLn = realRepoResolverLines(t, logPath)
	summary, _ := json.Marshal(rec)
	t.Logf("%s: %s", label, summary)
	return rec
}

// runRealRepoClean is the reference arm of the differential: the edited tree
// indexed from scratch into its own store (<store dir>/clean-<label>.sqlite,
// kept for a row-level comparison against the kept identity arms), exactly as
// the base corpus was indexed. An existing clean store is reused.
func runRealRepoClean(
	t *testing.T, tree, storeDir string, indexCfg config.IndexConfig, rel, edit, label string,
) realRepoExperiment {
	t.Helper()
	cleanPath := filepath.Join(storeDir, "clean-"+label+".sqlite")
	logPath := filepath.Join(storeDir, label+".log")
	rec := realRepoExperiment{File: rel, Mode: "clean", Edit: edit, LogPath: logPath, StepsMS: map[string]float64{}}
	if _, err := os.Stat(cleanPath); errors.Is(err, os.ErrNotExist) {
		restore := realRepoEdit(t, tree, rel, edit)
		defer restore()
		logger := realRepoLogger(t, logPath)
		store := builderOpenStoreAt(t, cleanPath+".tmp")
		idx := New(store, builderRegistry(), indexCfg, logger)
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		started := time.Now()
		if _, err := idx.Index(tree); err != nil {
			t.Fatalf("%s: index the edited tree: %v", label, err)
		}
		rec.ElapsedMS = ms(time.Since(started))
		idx.Close()
		if err := store.Close(); err != nil {
			t.Fatalf("%s: close the clean store: %v", label, err)
		}
		_ = logger.Sync()
		if err := os.Rename(cleanPath+".tmp", cleanPath); err != nil {
			t.Fatalf("%s: publish the clean store: %v", label, err)
		}
	}
	store, err := store_sqlite.Open(cleanPath)
	if err != nil {
		t.Fatalf("%s: open the clean store: %v", label, err)
	}
	rec.Identity = realRepoDurableDigest(t, store)
	_ = store.Close()
	return rec
}

// realRepoProfile runs fn under a CPU profile written to
// <store dir>/prof-<label>-<step>.pprof when GX_REALREPO_PROFILE=1.
func realRepoProfile(t testing.TB, storeDir, label, step string, fn func()) {
	t.Helper()
	if os.Getenv("GX_REALREPO_PROFILE") != "1" {
		fn()
		return
	}
	f, err := os.Create(filepath.Join(storeDir, "prof-"+label+"-"+step+".pprof"))
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	defer f.Close()
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatalf("start profile: %v", err)
	}
	defer pprof.StopCPUProfile()
	fn()
}

// realRepoResolverLines keeps the resolver's own records and the index pass's
// resolve line — every line whose message starts with "resolver:" or is the
// index pass's "indexer: resolve pass".
func realRepoResolverLines(t testing.TB, path string) []json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []json.RawMessage
	for _, line := range strings.Split(string(data), "\n") {
		var probe struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &probe) != nil {
			continue
		}
		if strings.HasPrefix(probe.Msg, "resolver: incremental phase") || probe.Msg == "resolver: compute progress" {
			continue
		}
		if strings.HasPrefix(probe.Msg, "resolver:") || probe.Msg == "indexer: resolve pass" {
			out = append(out, json.RawMessage(line))
		}
	}
	return out
}

// realRepoSurfaceDiff names, per referenceable declaration, the fields that
// differ between the file's nodes before and after the edit — the evidence for
// which Meta entries a body edit moves. Positions are reported separately as
// "position" so they are visible without being mistaken for surface.
func realRepoSurfaceDiff(before, after []*graph.Node) []string {
	byID := func(nodes []*graph.Node) map[string]*graph.Node {
		out := make(map[string]*graph.Node, len(nodes))
		for _, n := range nodes {
			if n != nil && n.Name != "" && graph.IsReferenceableSymbol(n.Kind) {
				out[n.ID] = n
			}
		}
		return out
	}
	old, cur := byID(before), byID(after)
	var diffs []string
	for id, o := range old {
		n, ok := cur[id]
		if !ok {
			diffs = append(diffs, "removed "+id)
			continue
		}
		var fields []string
		if o.StartLine != n.StartLine || o.EndLine != n.EndLine || o.StartColumn != n.StartColumn || o.EndColumn != n.EndColumn {
			fields = append(fields, "position")
		}
		if o.Kind != n.Kind || o.Name != n.Name || o.QualName != n.QualName || o.FilePath != n.FilePath || o.Language != n.Language {
			fields = append(fields, "identity")
		}
		keys := map[string]struct{}{}
		for k := range o.Meta {
			keys[k] = struct{}{}
		}
		for k := range n.Meta {
			keys[k] = struct{}{}
		}
		for k := range keys {
			a, _ := json.Marshal(o.Meta[k])
			b, _ := json.Marshal(n.Meta[k])
			if string(a) != string(b) {
				fields = append(fields, "meta."+k)
			}
		}
		if len(fields) > 0 {
			sort.Strings(fields)
			diffs = append(diffs, id+": "+strings.Join(fields, ","))
		}
	}
	for id := range cur {
		if _, ok := old[id]; !ok {
			diffs = append(diffs, "added "+id)
		}
	}
	sort.Strings(diffs)
	return diffs
}

// realRepoDurableDigest hashes, table by table in a total order, every durable
// row an incremental resolve can write: edges (every stored column but the
// rowid), nodes (every column but the wall-clock updated_at) and the reference
// facts. Equal digests across the two identity arms mean byte-identical rows.
func realRepoDurableDigest(t *testing.T, store *store_sqlite.Store) map[string]string {
	t.Helper()
	db := parityOpenRaw(t, store)
	queries := map[string]string{
		"edges": `SELECT from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier,
			cross_repo, view_gen, hex(meta), resolve_terminal, resolve_terminal_reason, semantic_source
			FROM edges ORDER BY from_id, to_id, kind, file_path, line, view_gen`,
		"nodes": `SELECT id, view_gen, kind, name, qual_name, file_path, start_line, end_line, start_column, end_column,
			language, repo_prefix, workspace_id, project_id, signature, visibility, doc, external, return_type,
			is_async, is_static, is_abstract, is_exported, data_class, clone_sig, hex(meta), semantic_type,
			semantic_source, entry_point, entry_point_kind
			FROM nodes ORDER BY id, view_gen`,
		"ref_facts": `SELECT * FROM ref_facts ORDER BY view_gen, repo_prefix, from_id, to_id, kind, line, file_path`,
	}
	out := make(map[string]string, len(queries)*2)
	for name, query := range queries {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("digest %s: %v", name, err)
		}
		cols, _ := rows.Columns()
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		h := sha256.New()
		count := 0
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("digest %s scan: %v", name, err)
			}
			for _, v := range values {
				fmt.Fprintf(h, "%v\x1f", v)
			}
			h.Write([]byte{'\n'})
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("digest %s rows: %v", name, err)
		}
		_ = rows.Close()
		out[name] = hex.EncodeToString(h.Sum(nil))
		out[name+"_rows"] = fmt.Sprint(count)
	}
	return out
}
