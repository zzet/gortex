package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A one-shot checkpoint lets a real accepted save land while the production
// attachment worker is alive. It changes scheduling only, never worker answers.
type daemonContractCheckpoint struct {
	entered, resume chan struct{}
	once            sync.Once
}

func (g *daemonContractCheckpoint) release() { g.once.Do(func() { close(g.resume) }) }

type daemonContractFixture struct {
	t       *testing.T
	ctx     context.Context
	state   *daemonState
	runtime *daemonContractAnalysis
	store   *store_sqlite.Store
	logs    *observer.ObservedLogs
	roots   map[string]string
	repos   []string
	gate    atomic.Pointer[daemonContractCheckpoint]
	callID  int
}

func newDaemonContractFixture(t *testing.T, sources map[string]map[string]string) *daemonContractFixture {
	t.Helper()
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	t.Setenv("GORTEX_TOOLS", "full")
	base := startupPublicationEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	f := &daemonContractFixture{t: t, ctx: ctx, roots: map[string]string{}}
	for repo := range sources {
		f.repos = append(f.repos, repo)
	}
	sort.Strings(f.repos)
	for _, repo := range f.repos {
		root := startupPublicationRepo(t, base, repo)
		f.roots[repo] = root
		for name, body := range sources[repo] {
			require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o600))
		}
	}
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	f.state = state
	f.store = state.graph.(*store_sqlite.Store)
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		// Never close SQLite while its independent worker still owns it.
		// Close explicitly accepts nil, so an installation refusal still
		// reaches shared.Close through this already-registered cleanup.
		if err := f.runtime.Close(closeCtx); err != nil {
			t.Errorf("contract worker drain: %v", err)
			return
		}
		require.NoError(t, state.shared.Close())
	})
	core, logs := observer.New(zapcore.InfoLevel)
	f.logs = logs
	runtime, err := installDaemonContractAnalysisWith(state, zap.New(core), func(options *indexer.ContractAnalysisCoordinatorOptions) {
		yield := options.Yield
		options.Yield = func(ctx context.Context) error {
			if gate := f.gate.Swap(nil); gate != nil {
				close(gate.entered)
				select {
				case <-gate.resume:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return yield(ctx)
		}
	})
	require.NoError(t, err)
	f.runtime = runtime
	for _, repo := range f.repos {
		_, err := state.lifecycle.Register(ctx, config.RepoEntry{Name: repo, Path: f.roots[repo], Workspace: "contract-functional", Project: "contract-functional"}, indexer.TrackSourceCLI)
		require.NoError(t, err)
	}
	return f
}

func (f *daemonContractFixture) counters() map[string]int64 {
	return viewmetrics.Default().Snapshot().Counters
}

func daemonContractSeries(counters map[string]int64, series string, labels ...string) int64 {
	var total int64
	for key, value := range counters {
		if key != series && !strings.HasPrefix(key, series+"{") {
			continue
		}
		matches := true
		for _, label := range labels {
			matches = matches && strings.Contains(key, label)
		}
		if matches {
			total += value
		}
	}
	return total
}

func (f *daemonContractFixture) idle() {
	f.t.Helper()
	// Registration may have admitted work before the complete cohort existed.
	// Check every possible actor of this small fixture, including startup
	// singletons/prefixes, rather than observing only the eventual full actor.
	require.LessOrEqual(f.t, len(f.repos), 3)
	require.Eventually(f.t, func() bool {
		for mask := 1; mask < 1<<len(f.repos); mask++ {
			var repos []string
			for i, repo := range f.repos {
				if mask&(1<<i) != 0 {
					repos = append(repos, repo)
				}
			}
			if f.runtime.coordinator.Status(indexer.NewContractActor("", repos)).State != "idle" {
				return false
			}
		}
		return true
	}, time.Minute, time.Millisecond, "attachment worker must finish before measuring the next logical edit")
}

func (f *daemonContractFixture) save(repo, name, body string) bool {
	f.t.Helper()
	before, found, err := f.store.ContractInputStateContext(f.ctx, repo, "")
	require.NoError(f.t, err)
	require.True(f.t, found)
	path := filepath.Join(f.roots[repo], name)
	require.NoError(f.t, os.WriteFile(path, []byte(body), 0o600))
	result, err := f.state.multiIndexer.IncrementalReindexRepo(repo, []string{path})
	require.NoError(f.t, err)
	require.Empty(f.t, result.FailedFiles)
	after, found, err := f.store.ContractInputStateContext(f.ctx, repo, "")
	require.NoError(f.t, err)
	require.True(f.t, found)
	return before.InputFingerprint != after.InputFingerprint || before.InputVersion != after.InputVersion
}

func (f *daemonContractFixture) publicContracts() []contracts.Contract {
	f.t.Helper()
	f.callID++
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": f.callID, "method": "tools/call",
		"params": map[string]any{"name": "contracts", "arguments": map[string]any{
			"action": "list", "type": "http", "all_repos": true, "limit": 1000,
			"format": "json", "wait_deadline": time.Now().Add(time.Minute).Format(time.RFC3339Nano),
		}},
	})
	require.NoError(f.t, err)
	reply, err := json.Marshal(f.state.mcpServer.MCPServer().HandleMessage(f.ctx, frame))
	require.NoError(f.t, err)
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(f.t, json.Unmarshal(reply, &envelope), string(reply))
	require.Nil(f.t, envelope.Error, string(reply))
	require.False(f.t, envelope.Result.IsError, string(reply))
	require.Len(f.t, envelope.Result.Content, 1, string(reply))
	text := envelope.Result.Content[0].Text
	var answer struct {
		Analysis struct {
			State string `json:"state"`
		} `json:"contract_analysis"`
		ByRepo map[string]struct {
			Contracts map[string][]contracts.Contract `json:"contracts"`
		} `json:"by_repo"`
	}
	require.NoError(f.t, json.Unmarshal([]byte(text), &answer), text)
	require.Equal(f.t, "complete", answer.Analysis.State, text)
	var records []contracts.Contract
	for _, repo := range answer.ByRepo {
		for _, rows := range repo.Contracts {
			records = append(records, rows...)
		}
	}
	return records
}

// Bind current authority independently after the public call. This tests actual
// freshness semantics; the richer contract rider's wire shape has its own tests.
func (f *daemonContractFixture) currentAnalysis(repo string) *graphview.ContractAnalysisView {
	f.t.Helper()
	m := f.state.mcpServer.Materializer()
	inputs, err := indexer.NewContractPrimaryInputCapture(m)(f.ctx, f.repos, repo)
	require.NoError(f.t, err)
	require.True(f.t, inputs.State.Accepted)
	analysis, err := m.OpenContractAnalysisForInputs(f.ctx, nil, inputs)
	require.NoError(f.t, err)
	f.t.Cleanup(analysis.Close)
	require.Equal(f.t, inputs.State.InputVersion, analysis.Attachment.InputVersion)
	require.Equal(f.t, inputs.State.InputFingerprint, analysis.Attachment.InputFingerprint)
	require.NoError(f.t, analysis.Validate(f.ctx))
	return analysis
}

func (f *daemonContractFixture) assertEditBudget(before map[string]int64, boundarySaves int, exactOne bool) {
	f.t.Helper()
	after := f.counters()
	delta := func(series string, labels ...string) int64 {
		return daemonContractSeries(after, series, labels...) - daemonContractSeries(before, series, labels...)
	}
	jobs := delta(viewmetrics.ContractAnalysisJobsTotal)
	require.Positive(f.t, jobs)
	require.LessOrEqual(f.t, jobs, int64(boundarySaves+1), "one finite key-moving writer cannot cause an unbounded job loop")
	if exactOne {
		require.EqualValues(f.t, 1, jobs)
		require.EqualValues(f.t, 1, delta(viewmetrics.ContractAnalysisJobsTotal, "mode=delta", "outcome=published"))
	}
	for _, mode := range []string{"cold", "fill"} {
		require.Zero(f.t, delta(viewmetrics.ContractAnalysisJobsTotal, "mode="+mode), "warm edit must use retained actor state")
	}
	for _, outcome := range []string{"failed", "panic", "superseded_read_unsettled"} {
		require.Zero(f.t, delta(viewmetrics.ContractAnalysisJobsTotal, "outcome="+outcome))
	}
	require.Zero(f.t, delta(viewmetrics.ContractAnalysisRequeuedTotal))
	files := delta(viewmetrics.ContractAnalysisFilesTotal)
	keyChecks := delta(viewmetrics.ContractAnalysisFilesTotal, "reason=key_check")
	require.LessOrEqual(f.t, files, 2+keyChecks, "route extraction and go.mod may not expand to the N200 census")
}

func TestDaemonContractN200WarmEditCorrectnessAndWorkBounds(t *testing.T) {
	routes := func(route string) string {
		return "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET " + route + "\", handle) }\n"
	}
	files := map[string]string{
		"main.go": "package fixture\n", "go.mod": "module example.test/contractfunctional\n\ngo 1.22\n",
		"routes.go":  routes("/before"),
		"handler.go": "package fixture\nimport \"net/http\"\nfunc handle(w http.ResponseWriter, r *http.Request) { response := Reply{Name:\"ok\"}; WriteJSON(w, 200, response) }\nfunc WriteJSON(w http.ResponseWriter, code int, body any) {}\n",
		"types.go":   "package fixture\ntype Reply struct { Name string `json:\"name\"` }\n",
	}
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("filler%03d.go", i)] = fmt.Sprintf("package fixture\nfunc Filler%d() int { return %d }\n", i, i)
	}
	f := newDaemonContractFixture(t, map[string]map[string]string{"tracked": files})
	initial := f.publicContracts()
	require.NotEmpty(t, initial, "the cold store must produce real contracts")
	require.Contains(t, fmt.Sprint(initial), "/before")
	warm := f.currentAnalysis("tracked")
	types, err := warm.NodesByKindsContext(f.ctx, []graph.NodeKind{graph.KindType})
	require.NoError(t, err)
	hasReplyShape := false
	for _, node := range types {
		if node.Name == "Reply" && node.Meta["shape"] != nil {
			hasReplyShape = true
		}
	}
	require.True(t, hasReplyShape, "the read-file rows require real handler-body/schema enrichment")
	warm.Close()
	f.idle()
	previous := "/before"
	for i, row := range []struct{ name, file, suffix string }{
		{"unread_comment", "filler099.go", "\n// concurrent unread comment\n"},
		{"unread_function", "filler099.go", "\nfunc UnreadAdded() {}\n"},
		{"read_comment", "handler.go", "\n// concurrent handler comment\n"},
		{"read_function", "handler.go", "\nfunc ReadAdded() {}\n"},
		{"route_comment", "routes.go", "\n// concurrent route comment\n"},
		{"route_function", "routes.go", "\nfunc RouteAdded() {}\n"},
		{"boundary_writer", "routes.go", ""},
	} {
		if !t.Run(row.name, func(t *testing.T) {
			parent := f.t
			f.t = t
			defer func() { f.t = parent }()
			before := f.counters()
			gate := &daemonContractCheckpoint{entered: make(chan struct{}), resume: make(chan struct{})}
			// A failed assertion must still release the real worker before cleanup.
			defer gate.release()
			f.gate.Store(gate)
			route := fmt.Sprintf("/edit-%d", i)
			files["routes.go"] = routes(route)
			require.True(t, f.save("tracked", "routes.go", files["routes.go"]))
			select {
			case <-gate.entered:
			case <-f.ctx.Done():
				t.Fatal("published route edit never entered an attachment checkpoint")
			}
			body := files[row.file] + row.suffix
			intermediate := route
			if row.name == "boundary_writer" {
				route += "-writer"
				body = routes(route)
			}
			moved := f.save("tracked", row.file, body)
			files[row.file] = body
			gate.release()
			started := time.Now()
			records := f.publicContracts()
			paths := map[string]bool{}
			for _, record := range records {
				paths[record.ID] = true
			}
			require.Contains(t, paths, "http::GET::"+route)
			require.NotContains(t, paths, "http::GET::"+previous)
			if row.name == "boundary_writer" {
				require.NotContains(t, paths, "http::GET::"+intermediate, "the overtaken route must disappear too")
			}
			analysis := f.currentAnalysis("tracked")
			nodes, err := analysis.NodesByKindsContext(f.ctx, []graph.NodeKind{graph.KindContract})
			require.NoError(t, err)
			require.NotEmpty(t, nodes)
			analysis.Close()
			f.idle()
			boundarySaves := 1
			if moved {
				boundarySaves++
			}
			f.assertEditBudget(before, boundarySaves, !moved)
			t.Logf("N200 %s complete with current input proof in %s; key-moving saves=%d", row.name, time.Since(started), boundarySaves)
			previous = route
		}) {
			return
		}
	}
}
