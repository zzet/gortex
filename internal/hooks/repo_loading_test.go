package hooks

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// On a warm restart the persisted catalog binds a configured repository before
// warmup registers it, so the daemon reports its files covered while every
// graph call refuses with repo_loading. Denying the native tool there would
// leave the agent with no read path at all.
func TestFileIndexStatusFromCoverage_LoadingIsNeverIndexed(t *testing.T) {
	st := fileIndexStatusFromCoverage(daemon.FileCoverageResult{
		Covered: true, Symbols: 12, Held: true, Answered: true, Tracked: true,
		RepoState: daemon.RepoStateLoading, Repo: "app",
	})
	if st.Indexed || !st.Loading || st.LoadingRepo != "app" {
		t.Fatalf("a loading repository's coverage must read as loading, not indexed: %#v", st)
	}

	ready := fileIndexStatusFromCoverage(daemon.FileCoverageResult{
		Covered: true, Symbols: 12, Held: true, Answered: true, Tracked: true,
	})
	if !ready.Indexed || ready.Loading {
		t.Fatalf("once the repository is ready the deny verdict must resume: %#v", ready)
	}
}

func TestLoadingRepo_NativeDoorsAllowed(t *testing.T) {
	stubFileIndexScope(t, fileIndexStatus{Loading: true, LoadingRepo: "app", Tracked: true, ProbeOK: true})

	read := enrichRead(map[string]any{"file_path": "internal/graph/graph.go"}, "/repo")
	if read.deny {
		t.Fatalf("Read of a loading repository's file must be allowed: %#v", read)
	}
	if !strings.Contains(read.context, "app is still loading") || !strings.Contains(read.context, "repo_loading") {
		t.Fatalf("the allowance must say why: %q", read.context)
	}

	if res := enrichEdit(map[string]any{"file_path": "internal/graph/graph.go"}, "/repo"); res.deny {
		t.Fatalf("Edit must be allowed while graph edits refuse with repo_loading: %#v", res)
	}
	if res := enrichBash(map[string]any{"command": "cat internal/graph/graph.go"}, "/repo"); res.deny {
		t.Fatalf("Bash cat must reach the same verdict as Read: %#v", res)
	}
}

func TestLoadingRepo_SearchScopeAllowed(t *testing.T) {
	hasSource, probeOK := scopeTrackedFromCoverage(daemon.DirCoverageResult{
		Answered: true, Tracked: true, HasSource: true, RepoState: daemon.RepoStateLoading,
	})
	if hasSource || !probeOK {
		t.Fatalf("a loading repository's scope must read as holding nothing the graph answers for: hasSource=%v probeOK=%v", hasSource, probeOK)
	}
}

func TestIdentityPending_NativeDoorsDoNotClaimOwnership(t *testing.T) {
	st := fileIndexStatusFromCoverage(daemon.FileCoverageResult{RepoState: daemon.RepoStateIdentityPending})
	if !st.Loading || st.Indexed || st.Tracked || st.LoadingRepo != "" {
		t.Fatalf("uncertain identity is not an indexed or tracked verdict: %#v", st)
	}
	stubFileIndexScope(t, st)
	for name, res := range map[string]enrichResult{
		"Read":     enrichRead(map[string]any{"file_path": "internal/graph/graph.go"}, "/repo"),
		"Edit":     enrichEdit(map[string]any{"file_path": "internal/graph/graph.go"}, "/repo"),
		"Bash cat": enrichBash(map[string]any{"command": "cat internal/graph/graph.go"}, "/repo"),
	} {
		if res.deny {
			t.Fatalf("%s denied while graph admission is unresolved: %#v", name, res)
		}
		if strings.Contains(res.context, "this repository is still loading") || strings.Contains(res.context, "already configured") {
			t.Fatalf("%s invented repository ownership: %s", name, res.context)
		}
	}
	read := enrichRead(map[string]any{"file_path": "internal/graph/graph.go"}, "/repo")
	if !strings.Contains(read.context, "identity_pending") {
		t.Fatalf("missing honest reason: %s", read.context)
	}
	hasSource, ok := scopeTrackedFromCoverage(daemon.DirCoverageResult{RepoState: daemon.RepoStateIdentityPending})
	if hasSource || !ok {
		t.Fatalf("pending directory must not redirect to graph: source=%v known=%v", hasSource, ok)
	}
}
