package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// What a long log costs the reads and the edits.
//
// A store is measured with an empty log, then with the log held at 256 MiB,
// 1, 2, 4 and 8 GiB by a pinned reader that began before the log grew (the
// case the reclaim cannot reset). Per size: the median of a fixed set of
// reads and of a fixed edit of five files, each against the empty-log
// baseline, and the writer's reads of the main file and of the log during the
// edit; a cost above a tenth over the baseline is flagged.
//
// Two stores:
//
//   - a real one: GORTEX_STORE_LOG_COST_STORE=<path> names an existing store,
//     a throwaway copy, opened as the daemon opens one (Open, no rebuild).
//     Nothing is built. Its keys come from the store itself by a fixed rule
//     (below) and are printed, so a second run reads the same ones. It writes
//     only to that copy: the edits and the log's growth re-add files of the
//     repository at a new revision.
//   - a synthetic one, when that variable is empty: a graph of
//     GORTEX_STORE_LOG_COST_FILES files (default 8,000: 240k nodes, 1.2M
//     edges) built in a temporary directory.
//
// The keys, for the repository GORTEX_STORE_LOG_COST_REPO (default "gortex"),
// in each generation of GORTEX_STORE_LOG_COST_GENS (default "0", the base; a
// second, e.g. "0,17" where 17 is a served generation of that store, adds the
// served generation's reads and a table of its own):
//
//   - 200 node ids of functions and methods: the ids sorted, taken at an even
//     stride;
//   - 50 names of those nodes (distinct, same stride), with the repository's
//     two most frequent languages;
//   - the out-edges of the 200 nodes with the most callers (in-edges);
//   - the nodes of 50 files of the repository's most frequent language
//     (the files sorted, even stride);
//   - the edit: 5 of those files (even stride), evicted and re-added with
//     their nodes and out-edges at a new revision.
//
// The log grows by re-adding 50 files of the repository at a time (stride
// through the sorted files). The reclaim is off (named override): the pinned
// reader alone holds the log, and no copy runs beside the measurement.
//
// Guards: it prints the store it opened, its size and free disk at the start;
// before each growth step and each measurement it stops, with the table so
// far, when free disk on the store's volume is under 15 GiB
// (GORTEX_STORE_LOG_COST_MIN_FREE_GIB). The store's own log lines are
// counted, not printed, so the output stays bounded.
//
// Gated: GORTEX_STORE_LOG_COST=1. GORTEX_STORE_LOG_COST_MAX_GIB caps the
// largest log size (default 8).
func TestLogSizeCostOfReadsAndEdits(t *testing.T) {
	if os.Getenv("GORTEX_STORE_LOG_COST") != "1" {
		t.Skip("set GORTEX_STORE_LOG_COST=1 (grows a log to 8 GiB)")
	}
	t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
	maxGiB := lcEnvInt("GORTEX_STORE_LOG_COST_MAX_GIB", 8)
	minFree := int64(lcEnvInt("GORTEX_STORE_LOG_COST_MIN_FREE_GIB", 15)) << 30
	repo := lcEnvString("GORTEX_STORE_LOG_COST_REPO", "gortex")

	// The store's own log lines are counted, not printed.
	logs := &lcLineCounter{}
	prevOut := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	var s *Store
	var path string
	var fileBatch func(file string, rev int) ([]*graph.Node, []*graph.Edge)
	realStore := os.Getenv("GORTEX_STORE_LOG_COST_STORE")
	if realStore != "" {
		path = realStore
		fi, err := os.Stat(path)
		require.NoError(t, err, "the store to measure")
		t.Logf("store: %s (existing, opened as the daemon opens one) size=%.2fGiB wal=%.2fGiB free_disk=%.1fGiB",
			path, float64(fi.Size())/(1<<30), float64(walFileSize(path+"-wal"))/(1<<30), float64(lcFreeDisk(t, path))/(1<<30))
		s, err = Open(path)
		require.NoError(t, err)
		// A file re-added as its stored rows at a new revision.
		fileBatch = func(file string, rev int) ([]*graph.Node, []*graph.Edge) {
			nodes := s.GetFileNodes(file)
			ids := make([]string, 0, len(nodes))
			for _, n := range nodes {
				ids = append(ids, n.ID)
				if n.Meta == nil {
					n.Meta = map[string]any{}
				}
				n.Meta["log_cost_rev"] = rev
			}
			var edges []*graph.Edge
			for _, es := range s.GetOutEdgesByNodeIDs(ids) {
				edges = append(edges, es...)
			}
			return nodes, edges
		}
	} else {
		path, fileBatch, s = lcSyntheticStore(t, repo)
	}
	base := s
	defer func() {
		started := time.Now()
		_ = base.Close()
		t.Logf("closed the store in %s (its final checkpoint of the log included); store log lines: %d", time.Since(started).Round(time.Second), logs.lines())
	}()
	lcAwaitStartupWork(t, s)
	ctx := context.Background()

	// The keys, by the fixed rule, for each measured generation (the base,
	// and optionally a served one: GORTEX_STORE_LOG_COST_GENS, e.g. "0,17").
	// A handle of one generation reads that generation's rows only, not the
	// chain composed over it. The edit and the log's growth write the base
	// generation, with the base's keys.
	var gens []int64
	for _, f := range strings.Split(lcEnvString("GORTEX_STORE_LOG_COST_GENS", "0"), ",") {
		g, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		require.NoError(t, err, "GORTEX_STORE_LOG_COST_GENS")
		gens = append(gens, g)
	}
	handles := map[int64]*Store{}
	keysOf := map[int64]lcKeys{}
	for _, g := range gens {
		h := s
		if g != s.viewGen {
			var err error
			h, err = s.AtManagedGeneration(g)
			require.NoError(t, err, "generation %d", g)
		}
		handles[g] = h
		k := lcChooseKeys(t, h, repo)
		keysOf[g] = k
		t.Logf("keys: repository=%s generation=%d functions=%d files=%d languages=%v", repo, g, k.population, len(k.allFiles), k.languages)
		t.Logf("keys: ids[0..4]=%v … (%d) names[0..4]=%v … (%d) hot[0..2]=%v … (%d)", k.ids[:min(5, len(k.ids))], len(k.ids), k.names[:min(5, len(k.names))], len(k.names), k.hot[:min(3, len(k.hot))], len(k.hot))
		t.Logf("keys: files[0..2]=%v … (%d) edit=%v", k.files[:min(3, len(k.files))], len(k.files), k.edit)
	}
	baseKeys, ok := keysOf[s.viewGen]
	if !ok {
		baseKeys = lcChooseKeys(t, s, repo)
	}

	type reads struct {
		byID, byNames, outEdges, fileNodes time.Duration
	}
	type sample struct {
		reads               map[int64]reads
		edit                time.Duration
		mainReads, walReads int64
	}
	median := func(xs []time.Duration) time.Duration {
		sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
		return xs[len(xs)/2]
	}
	rev := 1_000_000 + int(time.Now().Unix()%1000)*1000
	measure := func() sample {
		const reps = 5
		out := sample{reads: map[int64]reads{}}
		type laps struct{ a, b, c, d []time.Duration }
		perGen := map[int64]*laps{}
		var e []time.Duration
		var mains, wals []int64
		for r := 0; r < reps; r++ {
			for _, g := range gens {
				h, k := handles[g], keysOf[g]
				l := perGen[g]
				if l == nil {
					l = &laps{}
					perGen[g] = l
				}
				start := time.Now()
				require.NotEmpty(t, h.GetNodesByIDs(k.ids))
				l.a = append(l.a, time.Since(start))
				start = time.Now()
				_ = h.FindNodesByNamesInRepoLanguages(k.names, repo, k.languages)
				l.b = append(l.b, time.Since(start))
				start = time.Now()
				_ = h.GetOutEdgesByNodeIDs(k.hot)
				l.c = append(l.c, time.Since(start))
				start = time.Now()
				for _, p := range k.files {
					_ = h.GetFileNodes(p)
				}
				l.d = append(l.d, time.Since(start))
			}
			rev++
			before := s.ReaderWaitMark()
			start := time.Now()
			for _, f := range baseKeys.edit {
				n, ed := fileBatch(f, rev)
				s.EvictFile(f)
				s.AddBatch(n, ed)
			}
			e = append(e, time.Since(start))
			split := s.ReaderWaitMark().Split(before)
			mains = append(mains, split.VFS.WriterMainReads)
			wals = append(wals, split.VFS.WriterWALReads)
		}
		for g, l := range perGen {
			out.reads[g] = reads{median(l.a), median(l.b), median(l.c), median(l.d)}
		}
		sort.Slice(mains, func(i, j int) bool { return mains[i] < mains[j] })
		sort.Slice(wals, func(i, j int) bool { return wals[i] < wals[j] })
		out.edit, out.mainReads, out.walReads = median(e), mains[reps/2], wals[reps/2]
		return out
	}
	type row struct {
		label string
		log   int64
		s     sample
	}
	var rows []row
	printTable := func() {
		if len(rows) == 0 {
			return
		}
		ratio := func(x, b time.Duration) float64 {
			if b <= 0 {
				return 0
			}
			return float64(x) / float64(b)
		}
		for _, g := range gens {
			t.Logf("generation %d (reads of this generation; the edit writes the base, the same in every table):", g)
			t.Logf("%-8s %9s | %-18s %-18s %-18s %-18s %-18s | %9s %9s | %s",
				"log", "MiB", "node by id (200)", "names (50×2 lang)", "out-edges (200)", "file nodes (50)", "edit (5 files)", "w.main", "w.log", "over +10%")
			b := rows[0].s
			for _, r := range rows {
				x := r.s.reads[g]
				bb := b.reads[g]
				cells := []struct {
					x, b time.Duration
				}{{x.byID, bb.byID}, {x.byNames, bb.byNames}, {x.outEdges, bb.outEdges}, {x.fileNodes, bb.fileNodes}, {r.s.edit, b.edit}}
				names := []string{"by_id", "names", "out_edges", "file_nodes", "edit"}
				var parts, over []string
				for i, c := range cells {
					q := ratio(c.x, c.b)
					parts = append(parts, fmt.Sprintf("%9s ×%-6.2f", c.x.Round(100*time.Microsecond), q))
					if q > 1.10 {
						over = append(over, names[i])
					}
				}
				t.Logf("%-8s %9.0f | %s | %9d %9d | %s", r.label, float64(r.log)/(1<<20), strings.Join(parts, " "), r.s.mainReads, r.s.walReads, strings.Join(over, ","))
			}
		}
	}
	guard := func(step string) {
		if free := lcFreeDisk(t, path); free < minFree {
			printTable()
			t.Fatalf("stopped before %s: free disk %.1fGiB is under %.0fGiB; nothing more was written", step, float64(free)/(1<<30), float64(minFree)/(1<<30))
		}
	}

	guard("the baseline")
	_, err := s.writerDB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)
	rows = append(rows, row{"empty", walFileSize(path + "-wal"), measure()})
	// The pinned reader: began before the log grows, so nothing written after
	// it can be copied back and the log cannot restart.
	pinned, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = pinned.Rollback() }()
	var n int
	require.NoError(t, pinned.QueryRow(`SELECT count(*) FROM nodes WHERE rowid < 10`).Scan(&n))

	growStride := max(1, len(baseKeys.allFiles)/50)
	next := 0
	for _, target := range []int64{256 << 20, 1 << 30, 2 << 30, 4 << 30, 8 << 30} {
		if target > int64(maxGiB)<<30 {
			break
		}
		for walFileSize(path+"-wal") < target {
			guard(fmt.Sprintf("growing the log to %.2fGiB", float64(target)/(1<<30)))
			rev++
			var nodes []*graph.Node
			var edges []*graph.Edge
			for j := 0; j < 50; j++ {
				f := baseKeys.allFiles[(next*growStride+j*7)%len(baseKeys.allFiles)]
				nn, ee := fileBatch(f, rev)
				nodes, edges = append(nodes, nn...), append(edges, ee...)
			}
			next++
			s.AddBatch(nodes, edges)
		}
		guard(fmt.Sprintf("measuring at %.2fGiB", float64(target)/(1<<30)))
		rows = append(rows, row{fmt.Sprintf("%.2fGiB", float64(target)/(1<<30)), walFileSize(path + "-wal"), measure()})
		t.Logf("measured at log=%.0fMiB", float64(rows[len(rows)-1].log)/(1<<20))
	}
	printTable()
}

type lcKeys struct {
	population             int
	ids, names, hot, files []string
	edit, allFiles         []string
	languages              []string
}

// lcChooseKeys applies the fixed rule (see the test's comment).
func lcChooseKeys(t *testing.T, s *Store, repo string) lcKeys {
	t.Helper()
	var k lcKeys
	list := func(query string, args ...any) []string {
		rows, err := s.db.Query(query, args...)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			require.NoError(t, rows.Scan(&v))
			out = append(out, v)
		}
		require.NoError(t, rows.Err())
		return out
	}
	stride := func(all []string, n int) []string {
		if len(all) <= n {
			return all
		}
		out := make([]string, 0, n)
		step := float64(len(all)) / float64(n)
		for i := 0; i < n; i++ {
			out = append(out, all[int(float64(i)*step)])
		}
		return out
	}
	fns := list(`SELECT id FROM nodes WHERE view_gen = ? AND repo_prefix = ? AND kind IN ('function', 'method') ORDER BY id`, s.viewGen, repo)
	if len(fns) < 200 {
		prefixes := list(`SELECT repo_prefix FROM nodes WHERE view_gen = ? GROUP BY repo_prefix ORDER BY count(*) DESC LIMIT 10`, s.viewGen)
		t.Fatalf("repository %q has %d functions in generation %d; its repositories there: %v (GORTEX_STORE_LOG_COST_REPO)", repo, len(fns), s.viewGen, prefixes)
	}
	k.population = len(fns)
	k.ids = stride(fns, 200)
	byID := s.GetNodesByIDs(k.ids)
	seen := map[string]bool{}
	for _, id := range k.ids {
		if n := byID[id]; n != nil && n.Name != "" && !seen[n.Name] && len(k.names) < 50 {
			seen[n.Name] = true
			k.names = append(k.names, n.Name)
		}
	}
	k.languages = list(`SELECT language FROM nodes WHERE view_gen = ? AND repo_prefix = ? AND language <> '' GROUP BY language ORDER BY count(*) DESC, language LIMIT 2`, s.viewGen, repo)
	k.hot = list(`SELECT e.to_id FROM edges e JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
		WHERE e.view_gen = ? AND n.repo_prefix = ? GROUP BY e.to_id ORDER BY count(*) DESC, e.to_id LIMIT 200`, s.viewGen, repo)
	if len(k.languages) > 0 {
		k.allFiles = list(`SELECT DISTINCT file_path FROM nodes WHERE view_gen = ? AND repo_prefix = ? AND language = ? AND file_path <> '' ORDER BY file_path`, s.viewGen, repo, k.languages[0])
	}
	require.GreaterOrEqual(t, len(k.allFiles), 50, "files of %s in %s", k.languages, repo)
	k.files = stride(k.allFiles, 50)
	k.edit = stride(k.allFiles, 5)
	return k
}

// lcSyntheticStore builds the synthetic graph (GORTEX_STORE_LOG_COST_FILES
// files of 30 functions with five out-edges each) under the repository name
// the keys are read from.
func lcSyntheticStore(t *testing.T, repo string) (string, func(string, int) ([]*graph.Node, []*graph.Edge), *Store) {
	t.Helper()
	files := lcEnvInt("GORTEX_STORE_LOG_COST_FILES", 8000)
	const perFile = 30
	fileOf := func(g int) string { return fmt.Sprintf("%s/pkg%d/f%d.go", repo, g%97, g) }
	index := map[string]int{}
	for g := 0; g < files; g++ {
		index[fileOf(g)] = g
	}
	node := func(g, i, rev int) *graph.Node {
		file := fileOf(g)
		return &graph.Node{ID: fmt.Sprintf("%s::S%d", file, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("S%x_%d", (g*2654435761)%1000003, i), FilePath: file,
			RepoPrefix: repo, Language: []string{"go", "go", "python"}[g%3], StartLine: i * 10, EndLine: i*10 + 8,
			Meta: map[string]any{"rev": rev, "doc": fmt.Sprintf("symbol %d of file %d, revision %d", i, g, rev)}}
	}
	batch := func(g, rev int) ([]*graph.Node, []*graph.Edge) {
		nodes := make([]*graph.Node, perFile)
		for i := range nodes {
			nodes[i] = node(g, i, rev)
		}
		edges := make([]*graph.Edge, 0, 5*perFile)
		for i := range nodes {
			for e := 0; e < 5; e++ {
				to := node((g*31+e*17+i)%files, (i+e)%perFile, 0).ID
				edges = append(edges, &graph.Edge{From: nodes[i].ID, To: to, Kind: graph.EdgeCalls, FilePath: nodes[i].FilePath, Line: i*10 + e})
			}
		}
		return nodes, edges
	}
	s, path := openWALReclaimStore(t)
	for f := 0; f < files; f += 50 {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for g := f; g < min(f+50, files); g++ {
			n, e := batch(g, 0)
			nodes, edges = append(nodes, n...), append(edges, e...)
		}
		s.AddBatch(nodes, edges)
	}
	fi, err := os.Stat(path)
	require.NoError(t, err)
	t.Logf("store: %s (synthetic) files=%d nodes=%d edges=%d size=%.2fGiB free_disk=%.1fGiB", path, files, s.NodeCount(), s.EdgeCount(),
		float64(fi.Size())/(1<<30), float64(lcFreeDisk(t, path))/(1<<30))
	return path, func(file string, rev int) ([]*graph.Node, []*graph.Edge) { return batch(index[file], rev) }, s
}

// lcAwaitStartupWork waits for the store's startup work on the writer (row
// counters, the lazy index), which runs 30 s after an open.
func lcAwaitStartupWork(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		counters := !rowCountersEnabled() || s.rowCountersReady.Load()
		index := !lazyGraphIndexesEnabled() || s.fileGenerationIndexPresent()
		if counters && index {
			return
		}
		require.True(t, time.Now().Before(deadline), "startup work not done: row counters=%v lazy index=%v", counters, index)
		time.Sleep(time.Second)
	}
}

func lcFreeDisk(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Statfs_t
	dir := path
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		dir = path[:i]
	}
	require.NoError(t, syscall.Statfs(dir, &st))
	return int64(st.Bavail) * int64(st.Bsize)
}

func lcEnvInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func lcEnvString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// lcLineCounter swallows the store's log output and counts its lines.
type lcLineCounter struct {
	mu sync.Mutex
	n  int
}

func (c *lcLineCounter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.n += bytes.Count(p, []byte{'\n'})
	c.mu.Unlock()
	return len(p), nil
}

func (c *lcLineCounter) lines() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

var _ io.Writer = (*lcLineCounter)(nil)
