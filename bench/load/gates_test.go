package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadinessCurrentAndLegacyIdentityCoverage(t *testing.T) {
	expected := []privateRepo{{Path: "/private/primary", Corpus: Corpus{Label: "sample"}}}
	for _, data := range []string{
		`{"ready":true,"enriched":true,"tracked_repos":[{"path":"/private/primary","prefix":"sample","state":"ready"}],"loaded_roots":["/private/primary"]}`,
		`{"ready":true,"enriched":true,"tracked_repos":[{"path":"/private/primary","prefix":"sample"}]}`,
	} {
		var state readiness
		if err := json.Unmarshal([]byte(data), &state); err != nil {
			t.Fatal(err)
		}
		covered, err := readyCoverage(state, expected)
		if err != nil || !covered {
			t.Fatalf("protocol fixture rejected: %s %v", data, err)
		}
	}
	for _, data := range []string{
		`{"tracked_repos":[{"path":"/other","prefix":"sample","state":"ready"}],"loaded_roots":["/private/primary"]}`,
		`{"tracked_repos":[{"path":"/private/primary","prefix":"sample","state":"loading"}],"loaded_roots":[]}`,
		`{"tracked_repos":[{"path":"/private/primary","prefix":"sample","state":"ready"}],"loaded_roots":[]}`,
	} {
		var state readiness
		if err := json.Unmarshal([]byte(data), &state); err != nil {
			t.Fatal(err)
		}
		if covered, _ := readyCoverage(state, expected); covered {
			t.Fatalf("unloaded/mismatched root passed: %s", data)
		}
	}
	var failed readiness
	if err := json.Unmarshal([]byte(`{"tracked_repos":[{"path":"/private/primary","prefix":"sample","state":"failed","state_reason":"parse failed"}]}`), &failed); err != nil {
		t.Fatal(err)
	}
	if _, err := readyCoverage(failed, expected); err == nil {
		t.Fatal("failed corpus silently waited/passed")
	}
}

func TestColdQueryabilityAndEnrichmentObservedSeparately(t *testing.T) {
	started := time.Now()
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		return map[string]any{"ready": true, "enriched": time.Since(started) > 300*time.Millisecond, "tracked_repos": []any{map[string]any{"path": "/owned/repo", "prefix": "sample", "state": "ready"}}, "loaded_roots": []string{"/owned/repo"}}, nil
	})
	p := privateRun{Socket: socket, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gate, metrics, err := p.awaitReady(ctx, started, []privateRepo{{Path: "/owned/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	queryable, enriched := metrics["cold_queryable_observed_upper_bound_seconds"], metrics["cold_enrichment_observed_upper_bound_seconds"]
	if queryable.Value == nil || enriched.Value == nil || *queryable.Value >= *enriched.Value || gate["queryable_observation"] == nil {
		t.Fatalf("cold endpoints collapsed: %v %v", metrics, gate)
	}
}

func TestWorktreeBootConfigContainsOnlyPrimaryCorpus(t *testing.T) {
	root := t.TempDir()
	p := privateRun{Root: root, Repos: []privateRepo{{Path: "/owned/primary", Corpus: Corpus{Label: "sample"}}, {Path: "/owned/linked", Corpus: Corpus{Label: "linked-1"}}}}
	configured, err := p.writeBootRepos(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(configured) != 1 {
		t.Fatal("linked checkout became independent indexed corpus")
	}
	data, err := os.ReadFile(filepath.Join(root, "config", "gortex", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Repos []struct {
			Path string `json:"path"`
		} `json:"repos"`
	}
	if err := json.Unmarshal(data, &config); err != nil || len(config.Repos) != 1 || config.Repos[0].Path != "/owned/primary" {
		t.Fatalf("owned boot config: %s %v", data, err)
	}
}
