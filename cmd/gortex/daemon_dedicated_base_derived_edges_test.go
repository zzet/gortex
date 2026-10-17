package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// TestDaemonWarmupCommittedBaseCarriesTheDerivedEdges pins what a clean
// tree's committed base is a copy of: generation 0 after warmup's derived
// passes, not generation 0 at the readiness flip.
//
// The fixture's implements and accesses_field edges exist only once the
// batch's derived passes have run (infer_implements and capability_edges in
// end_batch). A base copied before them serves neither, so every checkout
// reading it loses them and every per-file delta over it re-derives them as
// rows the base does not hold. The claim-after-end_batch order is asserted
// directly (on this small fixture the asynchronous copy can lose the race to
// the batch, so the edge counts alone would not catch it). Revert-red: release
// the publisher at the readiness flip again (before end_batch).
func TestDaemonWarmupCommittedBaseCarriesTheDerivedEdges(t *testing.T) {
	base := startupPublicationEnv(t)
	root := startupPublicationRepo(t, base, "derived")
	source := "package a\n\ntype Setter interface{ Set() }\n\ntype Box struct{ n int }\n\nfunc (b *Box) Set() { b.n = 1 }\n\nfunc A() {}\n"
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	startupPublicationCommit(t, root)
	startupPublicationConsumer(t, base, root, "derived-dependent")

	core, observed := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	state, err := buildDaemonState(logger)
	if err != nil {
		t.Fatalf("buildDaemonState: %v", err)
	}
	t.Cleanup(func() {
		if state.shared != nil {
			_ = state.shared.Close()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	registered, err := state.lifecycle.Register(ctx, config.RepoEntry{Path: root}, indexer.TrackSourceCLI)
	if err != nil {
		t.Fatalf("register the repository: %v", err)
	}
	if err := state.lifecycle.Seed(ctx); err != nil {
		t.Fatalf("seed the checkout catalog: %v", err)
	}
	mw, _ := warmupDaemonState(state, logger, func() {})
	if mw != nil {
		t.Cleanup(func() { _ = mw.Stop() })
	}
	if err := waitForScheduledPublications(t, ctx, state); err != nil {
		t.Fatalf("wait for the scheduled publications: %v", err)
	}
	var published indexer.InitialBasePublication
	for _, outcome := range state.basePublisher.Outcomes() {
		if outcome.RepoPrefix == registered.Prefix {
			published = outcome
		}
	}
	if published.Err != nil || published.Skipped != "" || published.GenerationID <= 0 {
		t.Fatalf("no committed base was published: %+v", published)
	}
	// The order itself, which the edge counts below observe only when the
	// copy outruns the batch: the queue is released after end_batch.
	endBatch, released := -1, -1
	for i, entry := range observed.All() {
		switch {
		case entry.Message == "daemon: warmup phase done" && entry.ContextMap()["phase"] == "end_batch":
			endBatch = i
		case entry.Message == "daemon: committed-base publication released" && released < 0:
			released = i
		}
	}
	if endBatch < 0 || released < 0 {
		t.Fatalf("warmup logged no end_batch (%d) or no publication release (%d)", endBatch, released)
	}
	if released < endBatch {
		t.Errorf("committed-base publication was released (log entry %d) before end_batch finished (log entry %d): a copy can be taken before the derived passes", released, endBatch)
	}
	store, ok := state.graph.(*store_sqlite.Store)
	if !ok {
		t.Fatal("the daemon's backend is not the sqlite store")
	}
	dedicated := store.AtGeneration(published.GenerationID)
	for _, kind := range []graph.EdgeKind{graph.EdgeImplements, graph.EdgeAccessesField} {
		want := startupPublicationRepoEdges(store, registered.Prefix, kind)
		if want == 0 {
			t.Fatalf("fixture precondition: generation 0 holds no %s edge of %s", kind, registered.Prefix)
		}
		if got := startupPublicationRepoEdges(dedicated, registered.Prefix, kind); got != want {
			t.Errorf("the committed base serves %d %s edge(s) of %s, generation 0 holds %d: the base was copied before the derived passes",
				got, kind, registered.Prefix, want)
		}
	}
}

// startupPublicationCommit commits the fixture's working tree on top of its
// initial commit.
func startupPublicationCommit(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1",
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// startupPublicationRepoEdges counts the edges of one kind whose source file
// belongs to the repository.
func startupPublicationRepoEdges(r graph.Reader, repoPrefix string, kind graph.EdgeKind) int {
	n := 0
	for e := range r.EdgesByKind(kind) {
		if e != nil && strings.HasPrefix(e.FilePath, repoPrefix+"/") {
			n++
		}
	}
	return n
}
