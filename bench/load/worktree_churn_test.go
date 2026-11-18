package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStoreCensusCountsCopiesAndGoneCheckouts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	live := t.TempDir()
	for _, stmt := range []string{
		`CREATE TABLE nodes (id TEXT, view_gen INTEGER)`,
		`CREATE TABLE view_generations (state TEXT, generation_kind TEXT, storage_bytes INTEGER)`,
		`CREATE TABLE checkouts (state TEXT, effective_mode TEXT, admin_name TEXT, root_path TEXT)`,
		`INSERT INTO nodes VALUES ('a',0),('b',0),('a',7),('b',7)`,
		`INSERT INTO view_generations VALUES ('ready','dedicated',100),('ready','dedicated',50)`,
		`INSERT INTO checkouts VALUES ('ready','automatic','wt1','` + live + `'),('ready','automatic','wt2','/nonexistent/wt2'),('ready','automatic','','/primary')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
	_ = db.Close()
	census, err := storeCensus(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"census_node_rows_per_id":                 2,
		"census_node_view_gens":                   2,
		"census_generations.ready.dedicated":      2,
		"census_generation_bytes.ready.dedicated": 150,
		"census_checkouts.ready.automatic":        2,
		"census_checkouts_gone.ready":             1,
	}
	for name, value := range want {
		if m := census[name]; m.Value == nil || *m.Value != value {
			t.Errorf("%s = %+v, want %v", name, m, value)
		}
	}
}

func TestChurnCountersDiffReceipts(t *testing.T) {
	before := json.RawMessage(`{"views":{"counters":{"retired":2}}}`)
	after := json.RawMessage(`{"views":{"counters":{"retired":5,"refused":1},"retirement_backlog":{"generations":3,"bytes_estimate":4096}}}`)
	got := churnCounters([]json.RawMessage{before, after})
	for name, value := range map[string]float64{"counter.retired": 3, "counter.refused": 1, "retirement_backlog_generations": 3, "retirement_backlog_bytes": 4096} {
		if m := got[name]; m.Value == nil || *m.Value != value {
			t.Errorf("%s = %+v, want %v", name, m, value)
		}
	}
}

func TestChurnOptionsValidate(t *testing.T) {
	ok := churnOptions{Cycles: 4, Edits: 1, Idle: time.Second}
	if err := ok.validate("worktrees", 2, 0, nil); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func() error{
		"other scenario": func() error { return ok.validate("read", 2, 0, nil) },
		"with steps":     func() error { return ok.validate("worktrees", 2, 0, []int{0, 2}) },
		"fewer cycles":   func() error { return ok.validate("worktrees", 5, 0, nil) },
		"advance with editors": func() error {
			return churnOptions{Cycles: 4, Edits: 1, Advance: 2}.validate("worktrees", 2, 1, nil)
		},
		"reconcile only": func() error { return churnOptions{ReconcileInterval: time.Second}.validate("worktrees", 2, 0, nil) },
	} {
		if bad() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Setup failures must return before the monitor and the agents start: the
// run has no daemon process, so a started monitor would panic on its pid.
func TestWorktreeChurnSetupFailuresStartNothing(t *testing.T) {
	root := t.TempDir()
	p := &privateRun{Root: root, Socket: filepath.Join(root, "missing.sock"), Store: filepath.Join(root, "missing", "store.db"), Repos: []privateRepo{{Path: root}}}
	for name, churn := range map[string]churnOptions{
		"wal pin":  {Cycles: 1, Edits: 1, PinWAL: true},
		"mcp dial": {Cycles: 1, Edits: 1},
	} {
		o := runOptions{Worktrees: 1, Churn: churn}
		before := runtime.NumGoroutine()
		if err := measureWorktreeChurn(t.Context(), o, p, nil, &Phase{}, nil, ""); err == nil {
			t.Errorf("%s: setup failure not reported", name)
		}
		// database/sql's opener goroutine exits shortly after Close returns.
		for wait := 0; runtime.NumGoroutine() > before && wait < 100; wait++ {
			time.Sleep(10 * time.Millisecond)
		}
		if after := runtime.NumGoroutine(); after > before {
			t.Errorf("%s: %d goroutines left running", name, after-before)
		}
	}
}
