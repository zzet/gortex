package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/daemon"
)

// sampleStatus mimics a realistic StatusResponse so render functions
// can be exercised without running a live daemon.
func sampleStatus() daemon.StatusResponse {
	return daemon.StatusResponse{
		Version:       "v0.7.1",
		PID:           12345,
		SocketPath:    "/tmp/gortex.sock",
		UptimeSeconds: 180,
		Sessions:      1,
		MemoryBytes:   3_500_000_000,
		Ready:         true,
		WarmupSeconds: 42,
		TrackedRepos: []daemon.TrackedRepoStatus{
			{
				Prefix: "project1",
				Path:   "/tmp/code/project1",
				Files:  2029, Nodes: 20774, Edges: 208956,
				Memory: daemon.MemoryBreakdown{
					NodesBytes: 8_500_000, EdgesBytes: 63_000_000,
					SearchBytes: 17_000_000, VectorsBytes: 0,
					TotalBytes: 88_500_000,
				},
			},
			{
				Prefix: "project2",
				Path:   "/tmp/code/project2",
				Files:  6174, Nodes: 27578, Edges: 72190,
				Memory: daemon.MemoryBreakdown{
					NodesBytes: 12_000_000, EdgesBytes: 24_000_000,
					SearchBytes: 22_000_000, VectorsBytes: 0,
					TotalBytes: 58_000_000,
				},
			},
		},
	}
}

func TestRenderDaemonHeader_KeyFacts(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonHeader(&buf, sampleStatus())
	out := buf.String()
	for _, want := range []string{"daemon", "v0.7.1", "pid", "12345", "sessions", "ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("header output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderDaemonRepos_HasTableAndOtherRow(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonRepos(&buf, sampleStatus())
	out := buf.String()
	// Both repos appear, biggest-memory-first (project1 before project2).
	assert.Contains(t, out, "project1")
	assert.Contains(t, out, "project2")
	assert.Less(t, strings.Index(out, "project1"), strings.Index(out, "project2"),
		"repos should sort by memory desc")
	// "other" footer shows unattributed memory.
	assert.Contains(t, out, "other")
	assert.Contains(t, out, "embedder")
}

func TestRenderDaemonRepos_NoRepos(t *testing.T) {
	var buf bytes.Buffer
	renderDaemonRepos(&buf, daemon.StatusResponse{MemoryBytes: 100})
	assert.Contains(t, buf.String(), "tracked repos: (none)")
}

// TestRenderDaemonRepos_BusyAggregate — a status served while a track holds
// the controller carries the table from the last pass that computed one. The
// heading has to say so: an unmarked stale table is indistinguishable from a
// current one, and an unmarked empty one reads as "nothing is tracked".
func TestRenderDaemonRepos_BusyAggregate(t *testing.T) {
	t.Run("cached table names its age and the reason", func(t *testing.T) {
		now := time.Now()
		st := sampleStatus()
		st.AggregateBusy = true
		st.AggregateCachedUnix = now.Add(-90 * time.Second).Unix()
		assert.Equal(t,
			" (cached 1m30s ago — a track/reload is in progress)",
			daemonAggregateSuffix(st, time.Unix(st.AggregateCachedUnix+90, 0)))

		var buf bytes.Buffer
		renderDaemonRepos(&buf, st)
		out := buf.String()
		assert.Contains(t, out, "cached ")
		assert.Contains(t, out, "a track/reload is in progress")
		// The table itself still renders — a marked snapshot beats no answer.
		assert.Contains(t, out, "project1")
	})

	t.Run("clock skew never prints a negative age", func(t *testing.T) {
		st := sampleStatus()
		st.AggregateBusy = true
		st.AggregateCachedUnix = 5_000
		assert.Equal(t,
			" (cached 0s ago — a track/reload is in progress)",
			daemonAggregateSuffix(st, time.Unix(4_990, 0)))
	})

	t.Run("busy with no snapshot says so instead of (none)", func(t *testing.T) {
		var buf bytes.Buffer
		renderDaemonRepos(&buf, daemon.StatusResponse{AggregateBusy: true})
		out := buf.String()
		assert.Contains(t, out, "a track/reload is in progress")
		assert.NotContains(t, out, "(none)",
			"an unknown table must not be reported as an empty one")
	})

	t.Run("an uncontended status prints no suffix", func(t *testing.T) {
		var buf bytes.Buffer
		renderDaemonRepos(&buf, sampleStatus())
		out := buf.String()
		assert.Contains(t, out, "tracked repos:")
		assert.NotContains(t, out, "cached")
		assert.NotContains(t, out, "in progress")
	})
}

func TestRenderDaemonHeader_SearchBackendRow(t *testing.T) {
	st := sampleStatus()
	// What resolveSearchBackend emits for the store-native FTS index:
	// no heap figure of its own, so the row says disk-resident instead
	// of printing a fabricated "heap=0 B".
	st.SearchBackend = daemon.SearchBackendStats{
		Name:          "sqlite-fts5",
		DocCount:      65000,
		DocCountKnown: true,
		DiskResident:  true,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "sqlite-fts5")
	assert.Contains(t, out, "65000")
	assert.Contains(t, out, "disk-resident")
	assert.NotContains(t, out, "heap=")
}

func TestRenderDaemonHeader_SearchBackendRow_HeapBackend(t *testing.T) {
	st := sampleStatus()
	// The other arm of the row: a backend that is not disk-resident, so
	// the heap figure it does report has to be printed. resolveSearchBackend
	// reaches it for a backend it cannot identify, where the byte count
	// comes from search.BackendSize.
	st.SearchBackend = daemon.SearchBackendStats{
		Name:          "unknown",
		DocCount:      12000,
		DocCountKnown: true,
		Bytes:         200 * 1024 * 1024,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "unknown")
	assert.Contains(t, out, "12000")
	assert.Contains(t, out, "heap=")
}

func TestRenderDaemonHeader_WarmupLabel(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = false
	st.WarmupSeconds = 203
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "warmup 3m23s", "the state row must label total time-to-queryable as warmup, not resolve")
	assert.NotContains(t, out, "resolve 203s")
	assert.NotContains(t, out, "(resolve ")
}

func TestRenderDaemonHeader_EnrichmentProgress_NoSummary(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = false
	st.Enrichment = nil
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	assert.Contains(t, buf.String(), "enrichment in progress")
}

func TestRenderDaemonHeader_EnrichmentProgress_WithCurrent(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = false
	st.Enrichment = &daemon.EnrichmentProgress{
		Running:    true,
		ReposTotal: 22,
		ReposDone:  3,
		Current: &daemon.EnrichmentCurrent{
			Repo:            "gortex",
			Provider:        "gopls",
			ElapsedSeconds:  240,
			DeadlineSeconds: 900,
		},
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "enriching 3/22")
	assert.Contains(t, out, "gortex")
	assert.Contains(t, out, "4m0s/15m0s")
}

func TestRenderDaemonHeader_EnrichmentProgress_NoCurrentPass(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = false
	st.Enrichment = &daemon.EnrichmentProgress{
		Running:    false,
		ReposTotal: 5,
		ReposDone:  5,
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	assert.Contains(t, buf.String(), "enriching 5/5 repos")
}

// TestRenderDaemonHeader_LSPRouterSection — alive language servers are
// subprocesses the daemon holds open, and until they were rendered
// here the only way to see one leak was `ps`. The row reports the cap
// it is running against, the eviction count, and per provider the idle
// age plus the pin count that keeps a provider out of the reaper's
// reach.
func TestRenderDaemonHeader_LSPRouterSection(t *testing.T) {
	st := sampleStatus()
	st.LSPRouter = &daemon.LSPRouterStatus{
		MaxAlive:  6,
		Evictions: 3,
		ActiveProviders: []daemon.LSPActiveProvider{
			{
				Spec:      "gopls",
				Workspace: "/tmp/code/project1",
				LastUsed:  time.Now().Add(-3*time.Minute - 12*time.Second).Format(time.RFC3339),
			},
			{
				Spec:      "typescript-language-server",
				Workspace: "/tmp/code/project2",
				LastUsed:  time.Now().Add(-30 * time.Second).Format(time.RFC3339),
				InUse:     1,
			},
		},
	}
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "lsp")
	assert.Contains(t, out, "alive=2/6")
	assert.Contains(t, out, "evictions=3")
	assert.Contains(t, out, "gopls@/tmp/code/project1")
	assert.Contains(t, out, "3m12s ago")
	assert.Contains(t, out, "in_use=1")
}

// TestRenderDaemonHeader_LSPRouterOmittedWhenIdle — the section must
// vanish when no router is wired or nothing is alive, so daemons that
// never spawn a language server keep the terser header they had.
func TestRenderDaemonHeader_LSPRouterOmittedWhenIdle(t *testing.T) {
	for name, router := range map[string]*daemon.LSPRouterStatus{
		"no router wired": nil,
		"router with no alive provider": {
			MaxAlive:     6,
			EnabledSpecs: []daemon.LSPSpecStatus{{Name: "gopls", Available: true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := sampleStatus()
			st.LSPRouter = router
			var buf bytes.Buffer
			renderDaemonHeader(&buf, st)
			assert.NotContains(t, buf.String(), "alive=")
		})
	}
}

// TestFormatLSPLastUsed_UnparseablePassesThrough — a timestamp an
// older daemon build renders differently must stay visible rather than
// silently reading as "now".
func TestFormatLSPLastUsed_UnparseablePassesThrough(t *testing.T) {
	now := time.Now()
	assert.Equal(t, "not-a-timestamp", formatLSPLastUsed("not-a-timestamp", now))
	// Clock skew must not print a negative age.
	future := now.Add(5 * time.Second).Format(time.RFC3339)
	assert.Equal(t, "0s ago", formatLSPLastUsed(future, now))
}

func TestRenderDaemonHeader_ReadyAndEnriched_NoWarmupLabelChange(t *testing.T) {
	st := sampleStatus()
	st.Ready = true
	st.EnrichmentComplete = true
	st.EnrichSeconds = 300
	var buf bytes.Buffer
	renderDaemonHeader(&buf, st)
	out := buf.String()
	assert.Contains(t, out, "ready (warmup 5m0s)")
	assert.NotContains(t, out, "enrichment in progress")
}

// stubBuildVersion rewrites the ldflags-injected build identity for the
// duration of a subtest — the same `-X main.version` / `-X main.commit`
// seam goreleaser populates — so canonicalVersion() reports a chosen
// build and renderDaemonHeader's skew compare can be driven end-to-end.
func stubBuildVersion(t *testing.T, v, c string) {
	t.Helper()
	oldV, oldC := version, commit
	version, commit = v, c
	t.Cleanup(func() { version, commit = oldV, oldC })
}

// TestRenderDaemonHeader_SkewRow — the local-version row appears only
// when daemonSkewWarning(st.Version, canonicalVersion()) is non-empty,
// the same compare runProxy applies at connect time. Matching versions
// and dev builds (no injected identity, the v0.0.0-dev sentinel) must
// keep the table exactly as terse as it was.
func TestRenderDaemonHeader_SkewRow(t *testing.T) {
	t.Run("skewed versions append the cli row", func(t *testing.T) {
		stubBuildVersion(t, "0.63.3", "deadbee")
		st := daemon.StatusResponse{Version: "v0.63.4+abc1234"}
		// Precondition: the row is gated on this exact compare, so prove
		// the gate is live before asserting the render honors it.
		if daemonSkewWarning(st.Version, canonicalVersion()) == "" {
			t.Fatalf("expected daemonSkewWarning(%q, %q) to be non-empty",
				st.Version, canonicalVersion())
		}
		var buf bytes.Buffer
		renderDaemonHeader(&buf, st)
		out := buf.String()
		assert.Contains(t, out, "cli")
		assert.Contains(t, out, "v0.63.3+deadbee (differs from daemon)")
	})

	t.Run("matching versions omit the row", func(t *testing.T) {
		stubBuildVersion(t, "0.63.4", "abc1234")
		st := daemon.StatusResponse{Version: "v0.63.4+abc1234"}
		if daemonSkewWarning(st.Version, canonicalVersion()) != "" {
			t.Fatalf("expected daemonSkewWarning(%q, %q) to be empty",
				st.Version, canonicalVersion())
		}
		var buf bytes.Buffer
		renderDaemonHeader(&buf, st)
		assert.NotContains(t, buf.String(), "cli")
	})

	t.Run("dev build omits the row", func(t *testing.T) {
		// Plain `go build` identity: canonicalVersion() reports the
		// v0.0.0-dev sentinel, which daemonSkewWarning deliberately
		// ignores so dev binaries never nag about skew.
		stubBuildVersion(t, "0.0.0", "")
		assert.Equal(t, "v0.0.0-dev", canonicalVersion())
		st := daemon.StatusResponse{Version: "v0.63.4+abc1234"}
		var buf bytes.Buffer
		renderDaemonHeader(&buf, st)
		assert.NotContains(t, buf.String(), "cli")
	})
}

// TestRenderDaemonHeader_BinaryRow — the daemon's self-reported
// on-disk-binary drift row appears only when the drift probe ran
// (BinaryChecked) and found the running image stale. An unchecked binary
// must never render as stale — unknown is not stale.
func TestRenderDaemonHeader_BinaryRow(t *testing.T) {
	t.Run("stale binary appends the binary row", func(t *testing.T) {
		st := daemon.StatusResponse{
			Version:       "v0.63.4+abc1234",
			BinaryChecked: true,
			BinaryStale:   true,
		}
		var buf bytes.Buffer
		renderDaemonHeader(&buf, st)
		out := buf.String()
		assert.Contains(t, out, "binary")
		assert.Contains(t, out, "stale — on-disk image newer than running image")
	})

	t.Run("fresh binary omits the row", func(t *testing.T) {
		st := daemon.StatusResponse{
			Version:       "v0.63.4+abc1234",
			BinaryChecked: true,
		}
		var buf bytes.Buffer
		renderDaemonHeader(&buf, st)
		assert.NotContains(t, buf.String(), "binary")
	})

	t.Run("unchecked binary omits the row even if BinaryStale is set", func(t *testing.T) {
		st := daemon.StatusResponse{
			Version:     "v0.63.4+abc1234",
			BinaryStale: true,
		}
		var buf bytes.Buffer
		renderDaemonHeader(&buf, st)
		assert.NotContains(t, buf.String(), "binary")
	})
}
