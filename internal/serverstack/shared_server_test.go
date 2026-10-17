package serverstack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/persistence"
)

func TestSharedServerCloseReleasesNotebookSidecar(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lifecycle Lifecycle
	}{
		{"daemon", LifecycleDaemon},
		{"oneshot", LifecycleOneshot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "home")
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

			notebookPath := filepath.Join(base, "notebook")
			sidecarPath := persistence.DefaultSidecarPath(filepath.Join(notebookPath, ".gortex"))
			// Also release the handle if an assertion fails before stack teardown.
			t.Cleanup(func() { _ = persistence.CloseSidecar(sidecarPath) })
			conf := config.Default()
			conf.Semantic.Enabled = false
			stack, err := NewSharedServer(SharedServerConfig{
				Lifecycle:         tc.lifecycle,
				BackendPath:       filepath.Join(base, "store.sqlite"),
				Config:            conf,
				Logger:            zap.NewNop(),
				Embedder:          EmbedderRequest{FlagChanged: true, FlagEnabled: false},
				SideStores:        SideStores{NotebookPath: notebookPath},
				SavingsPath:       filepath.Join(base, "savings.sqlite"),
				SavingsLegacyJSON: filepath.Join(base, "savings.json"),
			})
			if err != nil {
				t.Fatalf("NewSharedServer: %v", err)
			}
			closed := false
			t.Cleanup(func() {
				if !closed {
					_ = stack.Close()
				}
			})

			if _, err := os.Stat(sidecarPath); err != nil {
				t.Fatalf("the stack did not create its notebook sidecar: %v", err)
			}
			// OpenSidecar returns the handle the notebook manager already owns.
			sidecar, err := persistence.OpenSidecar(sidecarPath)
			if err != nil {
				t.Fatalf("get notebook sidecar: %v", err)
			}
			repoKey := persistence.RepoCacheKey(notebookPath)
			entry := persistence.NotebookRow{ID: "close-regression", Body: "survives stack teardown"}
			if err := sidecar.UpsertNotebook(repoKey, entry); err != nil {
				t.Fatalf("write notebook: %v", err)
			}
			stack.cleanup = append(stack.cleanup, func() {
				if _, err := sidecar.LoadNotebookRows(repoKey); err != nil {
					t.Errorf("notebook sidecar closed before teardown finished: %v", err)
				}
			})

			if err := stack.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			closed = true
			if _, err := sidecar.LoadNotebookRows(repoKey); err == nil || !strings.Contains(err.Error(), "database is closed") {
				t.Fatalf("notebook handle after stack teardown: got %v, want a closed database", err)
			}
			reopened, err := persistence.OpenSidecar(sidecarPath)
			if err != nil {
				t.Fatalf("reopen notebook: %v", err)
			}
			if reopened == sidecar {
				t.Fatal("stack teardown left the closed notebook handle in the shared cache")
			}
			rows, err := reopened.LoadNotebookRows(repoKey)
			if err != nil || len(rows) != 1 || rows[0].ID != entry.ID || rows[0].Body != entry.Body {
				t.Fatalf("notebook after reopening: rows=%+v err=%v", rows, err)
			}
		})
	}
}

// TestNewSharedServer_Oneshot asserts the shared constructor builds a
// working stack over a tmp repo: the graph indexes, and the engine /
// MCP server / overlay manager are wired. This is the single-construction-
// path validation.
func TestNewSharedServer_Oneshot(t *testing.T) {
	repo := t.TempDir()
	src := "package toy\n\nfunc Add(a, b int) int { return a + b }\nfunc Mul(a, b int) int { return a * b }\n"
	if err := os.WriteFile(filepath.Join(repo, "toy.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ss, err := NewSharedServer(SharedServerConfig{
		Lifecycle: LifecycleOneshot,
		Index:     repo,
		// Private per-process store: never the shared default.
		BackendPath: filepath.Join(t.TempDir(), "embedded.sqlite"),
		Config:      config.Default(),
		Logger:      zap.NewNop(),
		Version:     "test",
		SideStores:  SideStores{NotesDir: t.TempDir(), NotesRepo: "test"},
		// Pin the savings ledger + legacy-import probe to temp paths:
		// with both empty the constructor opens the REAL machine-global
		// sidecar and imports (renaming!) the developer's real flat-file
		// ledger — a unit test must never mutate ~/.gortex.
		SavingsPath:       filepath.Join(t.TempDir(), "sidecar.sqlite"),
		SavingsLegacyJSON: filepath.Join(t.TempDir(), "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer: %v", err)
	}
	defer ss.Close()

	if ss.Graph == nil || ss.Indexer == nil || ss.Engine == nil || ss.MCP == nil {
		t.Fatalf("incomplete stack: graph=%v idx=%v eng=%v mcp=%v", ss.Graph != nil, ss.Indexer != nil, ss.Engine != nil, ss.MCP != nil)
	}
	if ss.Overlays == nil {
		t.Error("overlay manager should be wired")
	}

	if _, err := ss.Indexer.Index(repo); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if n := ss.Graph.Stats().TotalNodes; n == 0 {
		t.Fatal("graph should be non-empty after indexing the tmp repo")
	}
}

// TestNewSharedServer_LifecyclesDefaultToSqlite asserts every lifecycle
// accepts the empty backend name (there is only the sqlite store), and that
// one-shot alone stays outside the store lock.
func TestNewSharedServer_LifecyclesDefaultToSqlite(t *testing.T) {
	if err := checkBackend(""); err != nil {
		t.Errorf("the empty backend name must resolve: %v", err)
	}
	if LifecycleOneshot.Writable() {
		t.Error("oneshot must not be writable (no store lock)")
	}
	if !LifecycleDaemon.Writable() {
		t.Error("the daemon lifecycle owns a durable store")
	}
}

// TestNewSharedServer_OneshotRefusesSharedStore pins the safety property
// that keeps the unlocked embedded server off the daemon's database: with
// no BackendPath the sqlite path would resolve to ~/.gortex/store, so the
// constructor must refuse instead of opening it.
func TestNewSharedServer_OneshotRefusesSharedStore(t *testing.T) {
	_, err := NewSharedServer(SharedServerConfig{
		Lifecycle: LifecycleOneshot,
		Index:     t.TempDir(),
		Config:    config.Default(),
		Logger:    zap.NewNop(),
	})
	if err == nil {
		t.Fatal("one-shot without BackendPath must be refused, not pointed at the shared store")
	}
	if !strings.Contains(err.Error(), "BackendPath") {
		t.Errorf("error should name the missing BackendPath, got: %v", err)
	}
}

// TestSharedServerInstallsOneOutputGenerationAuthorityOnBothLanes is the
// production-entrypoint trace for the output-generation authority's install.
//
// NewSharedServer is the one constructor both production entry points build
// through (cmd/gortex/daemon_state.go for the daemon, cmd/gortex/mcp.go for
// the embedded one-shot server). It always builds a standalone Indexer and
// hands it straight to the MCP server; that Indexer mints an ORPHAN mutation
// lane, which is the lane the MultiIndexer's batch gate does not span. The
// authority is what makes both lanes name their output generation and owner
// through one process-wide fence, so the install has to happen here rather
// than in cmd/gortex/daemon.go — the SetBuildGate asymmetry that leaves the
// one-shot path ungated is the counter-example.
func TestSharedServerInstallsOneOutputGenerationAuthorityOnBothLanes(t *testing.T) {
	stack, closeStack := newPublisherStack(t)

	authority := stack.OutputGenerationAuthority
	if authority == nil {
		t.Fatal("the stack published no output-generation authority")
	}
	if stack.Indexer == nil {
		t.Fatal("the stack built no standalone Indexer")
	}
	// The standalone Indexer — the orphan lane — resolves to the stack's
	// authority, not to a private one.
	if got := stack.Indexer.ResolvedOutputGenerationAuthority(); got != authority {
		t.Fatal("the standalone Indexer does not resolve to the stack's output-generation authority")
	}
	if stack.MultiIndexer == nil {
		t.Fatal("the stack built no MultiIndexer")
	}
	// Every owned per-repository lane resolves to the SAME value, so a
	// mutation raised through the standalone Indexer and one raised through an
	// owned lane are ordered by one authority rather than two.
	owned := stack.MultiIndexer.ResolvedOutputGenerationAuthority()
	if owned != authority {
		t.Fatal("the MultiIndexer does not resolve to the stack's output-generation authority")
	}
	// The authority witnesses source through the lease manager a routed
	// request's base pin is taken from. A private manager would make a
	// generation-zero mutation invisible to the request that has to hear
	// about it.
	if stack.CheckoutLifecycle != nil {
		if authority.ViewLeases() != stack.CheckoutLifecycle.ViewLeases() {
			t.Fatal("the authority was installed with a lease manager the lifecycle does not share")
		}
		// The same manager must also be the materializer's: a routed request's
		// BasePin is taken from Materializer.Leases, so a generation-zero
		// mutation witnessed through any other manager is invisible to the
		// request that has to hear about it, and the source half of the
		// authority is silently inert again.
		if stack.MCP != nil && stack.MCP.Materializer() != nil {
			if stack.MCP.Materializer().Leases != authority.ViewLeases() {
				t.Fatal("a routed request's base pin is taken from a lease manager the authority does not witness through")
			}
		}
	}

	// Teardown stops admission.
	closeStack()
	if _, err := authority.Begin(context.Background(), indexer.OutputEntryIndexFile, indexer.OutputMutationTarget{
		Kind: indexer.OutputGenerationLegacy, OwnerKey: "root:/tmp/after-close",
	}); !errors.Is(err, indexer.ErrOutputMutationAuthorityClosed) {
		t.Fatalf("admission after stack teardown: got %v, want ErrOutputMutationAuthorityClosed", err)
	}
}
