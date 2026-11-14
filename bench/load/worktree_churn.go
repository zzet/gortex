package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Worktree churn mode measures what removed linked worktrees leave behind.
//
// Readers and editors work the primary for the whole run, two sessions stay
// parked on it, and one more session repeats a whole-graph analysis. Over that
// load the mode links --worktree-churn worktrees one at a time, keeping at most
// --worktrees alive: when that many are live the oldest is closed and removed
// first, then the new one is linked at one of four commits of the corpus
// history and gets a session and --churn-edits edits. After the last cycle
// every worktree is removed, the daemon sits
// idle for --churn-idle, and a read-only census of the store reports what is
// still there. A daemon that retires removed checkouts ends near one node row
// per node id; one that keeps them ends with a copy per kept generation.

type churnOptions struct {
	Cycles            int
	Edits             int
	Advance           int
	PinWAL            bool
	Idle              time.Duration
	ReconcileInterval time.Duration
}

func bindChurnFlags(f *flag.FlagSet, o *churnOptions) {
	f.IntVar(&o.Cycles, "worktree-churn", 0, "worktrees scenario: link and remove this many worktrees over the load, at most --worktrees alive at once")
	f.IntVar(&o.Edits, "churn-edits", 3, "worktree churn: edits per worktree before its commit")
	f.IntVar(&o.Advance, "churn-advance", 0, "worktree churn: commit a change on the primary every this many cycles (0 never); needs --edit-agents 0")
	f.BoolVar(&o.PinWAL, "churn-pin-wal", false, "worktree churn: hold one read transaction on the store from the first cycle to the live census, so the WAL cannot reset (stands in for a long reader)")
	f.DurationVar(&o.Idle, "churn-idle", 2*time.Minute, "worktree churn: idle window after the last removal, before the census")
	f.DurationVar(&o.ReconcileInterval, "reconcile-interval", 0, "worktree churn: GORTEX_RECONCILE_INTERVAL for the owned daemon (0 keeps the daemon default)")
}

func (o churnOptions) validate(scenario string, worktrees, editors int, steps []int) error {
	if o.Cycles == 0 {
		if o.ReconcileInterval != 0 {
			return errors.New("--reconcile-interval applies to --worktree-churn")
		}
		return nil
	}
	if scenario != "worktrees" || len(steps) > 0 || worktrees < 1 || o.Cycles < worktrees || o.Edits < 1 || o.Idle < 0 || o.ReconcileInterval < 0 || o.Advance < 0 {
		return errors.New("--worktree-churn needs the worktrees scenario without --worktree-steps, --worktrees >= 1 live at once, at least that many cycles and --churn-edits >= 1")
	}
	if o.Advance > 0 && editors > 0 {
		// An editor's edit races the commit and is refused as stale.
		return errors.New("--churn-advance needs --edit-agents 0")
	}
	return nil
}

type churnWorktree struct {
	repo   privateRepo
	client *socketClient
}

func measureWorktreeChurn(ctx context.Context, o runOptions, p *privateRun, clients []*socketClient, phase *Phase, sampler processSampler, group string) error {
	phase.SessionModel = fmt.Sprintf("per_agent_with_worktree_churn:%d/%d", o.Churn.Cycles, o.Worktrees)
	// Everything that can fail is set up before the monitor and the agents
	// start: from there on every path below stops and joins both.
	if o.Churn.Advance > 0 {
		if err := os.WriteFile(filepath.Join(p.Root, "gitconfig"), []byte("[user]\n\tname = load\n\temail = load@invalid\n"), 0o600); err != nil {
			return err
		}
	}
	var pin *sql.Tx
	if o.Churn.PinWAL {
		db, err := sql.Open("sqlite", "file:"+p.Store+"?mode=ro")
		if err != nil {
			return err
		}
		defer db.Close()
		if pin, err = db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err != nil {
			return err
		}
		defer func() { _ = pin.Rollback() }()
		var generations int
		if err := pin.QueryRowContext(ctx, `SELECT count(*) FROM view_generations`).Scan(&generations); err != nil {
			return err
		}
	}

	var extra []*socketClient
	defer func() { closeClients(extra) }()
	for range 3 { // two parked sessions and the analysis session
		client, err := openMCP(ctx, p.Socket, p.Repos[0].Path)
		if err != nil {
			return err
		}
		extra = append(extra, client)
	}

	phase.Started = time.Now()
	monitor := startMonitor(p, sampler, group)
	// The churn loop records into its own phase: the agents append to phase
	// concurrently under a lock this loop does not share.
	churn := Phase{Gate: make(map[string]json.RawMessage), Metrics: make(map[string]Metric)}
	agentCtx, stopAgents := context.WithCancel(ctx)
	var agents sync.WaitGroup
	agentOptions := o
	agentOptions.Duration = 24 * time.Hour // the churn decides when the load ends
	agents.Go(func() { _ = runAgents(agentCtx, agentOptions, p, clients, o.Editors > 0, phase) })
	var analyses []Attempt
	agents.Go(func() {
		r := request{Tool: "analyze", Operation: "dead_code", Arguments: map[string]any{"kind": "dead_code"}}
		for step := 0; agentCtx.Err() == nil; step++ {
			_, rows, _ := callTool(agentCtx, extra[2], r, dispatchArgs(r, p.Repos[0]), "workload", -1, step, 0, o.Deadline, waitContext)
			analyses = append(analyses, rows...)
			if waitContext(agentCtx, 2*time.Second) != nil {
				return
			}
		}
	})

	var live []churnWorktree
	removeOldest := func() error {
		w := live[0]
		live = live[1:]
		_ = w.client.Close()
		_, err := p.git(ctx, p.Repos[0].Path, "worktree", "remove", "--force", w.repo.Path)
		return err
	}
	churnErr := func() error {
		for i := range o.Churn.Cycles {
			for len(live) >= o.Worktrees { // make room first, so at most --worktrees are ever alive
				if err := removeOldest(); err != nil {
					return err
				}
			}
			w, err := linkChurnWorktree(ctx, o, p, i, &churn)
			if w.client != nil {
				live = append(live, w)
			}
			if err != nil {
				return err
			}
			if o.Churn.Advance > 0 && i%o.Churn.Advance == o.Churn.Advance-1 {
				if err := advancePrimary(ctx, p, i); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	if churnErr == nil {
		captureReceipt(ctx, p, extra[0], &churn) // the daemon's own view while worktrees are live
		var census map[string]Metric
		census, churnErr = storeCensus(p.Store)
		for name, metric := range census {
			churn.Metrics["live_"+name] = metric
		}
	}
	if pin != nil {
		_ = pin.Rollback()
	}
	for churnErr == nil && len(live) > 0 {
		churnErr = removeOldest()
	}
	stopAgents()
	agents.Wait()
	phase.Attempts = append(phase.Attempts, analyses...)
	phase.Attempts = append(phase.Attempts, churn.Attempts...)
	phase.Edits = append(phase.Edits, churn.Edits...)
	finishEditMetrics(phase)
	for name, value := range churn.Gate {
		phase.Gate[name] = value
	}
	for name, metric := range churn.Metrics {
		phase.Metrics[name] = metric
	}
	for i, status := range churn.Status {
		phase.Gate[fmt.Sprintf("live_status.%d", i)] = status
	}
	phase.DurationSec = time.Since(phase.Started).Seconds()
	if churnErr != nil {
		monitor.finish(phase)
		return churnErr
	}
	idleErr := waitContext(ctx, o.Churn.Idle)
	monitor.finish(phase)
	captureReceipt(ctx, p, extra[0], phase)
	for name, metric := range churnCounters(phase.Status) {
		phase.Metrics[name] = metric
	}
	census, err := storeCensus(p.Store)
	for name, metric := range census {
		phase.Metrics[name] = metric
	}
	return errors.Join(idleErr, err)
}

// churnCommitStride spaces the commits worktrees are linked at: worktree i
// checks out the pinned commit's (i mod 4)*stride-th first-parent ancestor, so
// each one owns a commit layer that differs from the base.
const churnCommitStride = 8

// linkChurnWorktree links worktree i, waits for its exact view, then edits it
// so it owns a dirty layer on top of its commit layer.
func linkChurnWorktree(ctx context.Context, o runOptions, p *privateRun, i int, churn *Phase) (churnWorktree, error) {
	var w churnWorktree
	warmCtx, cancel := context.WithTimeout(ctx, o.WarmTimeout)
	defer cancel()
	primary := p.Repos[0]
	label := fmt.Sprintf("churn-%d", i)
	path := filepath.Join(p.Root, "repos", label)
	commit := fmt.Sprintf("%s~%d", primary.Corpus.Commit, (i%4)*churnCommitStride)
	if _, err := p.git(warmCtx, primary.Path, "worktree", "add", "--detach", path, commit); err != nil {
		return w, err
	}
	file, err := ownedFile(path, primary.Corpus.File)
	if err != nil {
		return w, err
	}
	w.repo = privateRepo{Corpus: primary.Corpus, Path: path, File: file, Prefix: primary.Prefix}
	w.repo.Corpus.Label = label
	if w.client, err = openMCP(warmCtx, p.Socket, path); err != nil {
		return w, err
	}
	if err := awaitChurnView(warmCtx, p, w.repo, o.Deadline, churn); err != nil {
		return w, err
	}
	for step := range o.Churn.Edits {
		observation, rows := editAndObserve(warmCtx, o, w.client, w.repo, 1000+i, step) // markers distinct from the primary's agents
		churn.Edits = append(churn.Edits, observation)
		churn.Attempts = append(churn.Attempts, rows...)
	}
	return w, warmCtx.Err()
}

// awaitChurnView waits until the daemon answers for the worktree from its own
// exact view. Until then it may answer unrouted, building or from the base;
// how long that took and how often the base answered are kept per worktree.
func awaitChurnView(ctx context.Context, p *privateRun, repo privateRepo, deadline time.Duration, churn *Phase) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	started, served := time.Now(), map[string]int{}
	for retry := 0; ; retry++ {
		data, err := control(ctx, p.Socket, "search_symbols", map[string]any{"path": repo.Path, "query": repo.Corpus.Query, "limit": 1})
		if err == nil {
			var answer struct {
				View *struct {
					Kind  string `json:"kind"`
					Exact bool   `json:"exact"`
				} `json:"view"`
			}
			if json.Unmarshal(data, &answer) == nil && answer.View != nil {
				served[answer.View.Kind]++
				if answer.View.Exact && answer.View.Kind == "worktree" {
					churn.Gate[repo.Corpus.Label+".view_wait"], _ = json.Marshal(map[string]any{"ms": time.Since(started).Milliseconds(), "served": served})
					return nil
				}
			}
		}
		if err := waitContext(ctx, busyBackoff(retry)); err != nil {
			return fmt.Errorf("%s: no exact worktree view after %s (served %v): %w", repo.Corpus.Label, time.Since(started).Round(time.Millisecond), served, err)
		}
	}
}

// advancePrimary commits one comment line on the primary, moving its HEAD to
// a new tree while the live worktrees still sit on the old ones.
func advancePrimary(ctx context.Context, p *privateRun, i int) error {
	primary := p.Repos[0]
	file, err := os.OpenFile(primary.File, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "\n// load churn advance %d\n", i)
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	_, err = p.git(ctx, primary.Path, "commit", "--quiet", "--all", "--message", fmt.Sprintf("load churn advance %d", i))
	return err
}

// churnCounters reports the retirement and build-lane counters the first and
// last status receipts carry, as deltas, plus the backlog the last one shows.
func churnCounters(receipts []json.RawMessage) map[string]Metric {
	type views struct {
		Views struct {
			Counters map[string]int64 `json:"counters"`
			Backlog  *struct {
				Generations   int     `json:"generations"`
				BytesEstimate int64   `json:"bytes_estimate"`
				DebtAge       float64 `json:"debt_age_seconds"`
			} `json:"retirement_backlog"`
		} `json:"views"`
	}
	out := make(map[string]Metric)
	if len(receipts) < 2 {
		out["churn_counters"] = unavailable("count", "fewer than two daemon status receipts")
		return out
	}
	var before, after views
	if json.Unmarshal(receipts[0], &before) != nil || json.Unmarshal(receipts[len(receipts)-1], &after) != nil {
		out["churn_counters"] = unavailable("count", "undecodable daemon status receipt")
		return out
	}
	// Status omits zero-valued counter series and an empty backlog, so a
	// missing key is an observed zero, not an unavailable one.
	for key, value := range after.Views.Counters {
		out["counter."+key] = measured(float64(value-before.Views.Counters[key]), "count")
	}
	backlog, bytes := 0.0, 0.0
	if b := after.Views.Backlog; b != nil {
		backlog, bytes = float64(b.Generations), float64(b.BytesEstimate)
	}
	out["retirement_backlog_generations"] = measured(backlog, "generations")
	out["retirement_backlog_bytes"] = measured(bytes, "bytes")
	return out
}

// storeCensus reads the store's size and what it still holds, on a read-only
// connection: one short reader, opened after the load has ended.
func storeCensus(path string) (map[string]Metric, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	out := make(map[string]Metric)
	var pages, free, pageSize, rows, ids, gens float64
	if err := db.QueryRow(`SELECT (SELECT page_count FROM pragma_page_count), (SELECT freelist_count FROM pragma_freelist_count), (SELECT page_size FROM pragma_page_size)`).Scan(&pages, &free, &pageSize); err != nil {
		return nil, err
	}
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT id), count(DISTINCT view_gen) FROM nodes`).Scan(&rows, &ids, &gens); err != nil {
		return nil, err
	}
	out["census_store_bytes"] = measured(pages*pageSize, "bytes")
	out["census_freelist_bytes"] = measured(free*pageSize, "bytes")
	out["census_node_rows"] = measured(rows, "rows")
	out["census_node_view_gens"] = measured(gens, "generations")
	out["census_node_rows_per_id"] = unavailable("ratio", "no node rows")
	if ids > 0 {
		out["census_node_rows_per_id"] = measured(rows/ids, "ratio")
	}
	groups := []struct{ name, unit, query string }{
		{"census_generations", "count", `SELECT state || '.' || generation_kind, count(*) FROM view_generations GROUP BY 1`},
		{"census_generation_bytes", "bytes", `SELECT state || '.' || generation_kind, sum(storage_bytes) FROM view_generations GROUP BY 1`},
		{"census_checkouts", "count", `SELECT state || '.' || effective_mode, count(*) FROM checkouts WHERE admin_name <> '' GROUP BY 1`},
	}
	for _, g := range groups {
		result, err := db.Query(g.query)
		if err != nil {
			return out, err
		}
		for result.Next() {
			var key string
			var value float64
			if err := result.Scan(&key, &value); err != nil {
				_ = result.Close()
				return out, err
			}
			out[g.name+"."+key] = measured(value, g.unit)
		}
		if err := errors.Join(result.Err(), result.Close()); err != nil {
			return out, err
		}
	}
	// A linked checkout whose directory is gone but whose row remains is what
	// removal detection has not caught up with yet.
	gone, err := db.Query(`SELECT state, root_path FROM checkouts WHERE admin_name <> ''`)
	if err != nil {
		return out, err
	}
	defer gone.Close()
	for gone.Next() {
		var state, root string
		if err := gone.Scan(&state, &root); err != nil {
			return out, err
		}
		if _, statErr := os.Stat(root); errors.Is(statErr, os.ErrNotExist) {
			key := "census_checkouts_gone." + state
			value := 1.0
			if m := out[key]; m.Value != nil {
				value += *m.Value
			}
			out[key] = measured(value, "count")
		}
	}
	return out, gone.Err()
}
