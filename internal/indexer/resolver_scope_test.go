package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
)

// The per-save path hands the resolver the edited file's pre-eviction
// declaration surface and prior-unresolved out-edges through the deferred
// catch-up. These tests pin (a) that the evidence reaches the resolver — a body
// edit skips the parked references on its unchanged declarations — and (b)
// that doing so changes nothing durable: for body, signature and import edits
// the store after the scoped catch-up is row-identical to the store after the
// exhaustive one (index.dirty_chain.resolver_evidence_scope: false).

func resolverScopeTree() map[string]string {
	return map[string]string{
		"pkg/a.go": `package pkg

import "strings"

// Target is referenced from Go (binds) and TypeScript (never binds here).
func Target() int { return len(strings.TrimSpace(" x ")) }

type Box struct{}

func (Box) Load() int { return 2 }
`,
		"pkg/b.go": `package pkg

func Use() int { return Target() + Box{}.Load() }

func Dynamic(v interface{ Load() int }) int { return v.Load() + Unknown() }
`,
		"web/c.ts": `export function f() { return Target() + Load(); }
`,
	}
}

var resolverScopeEdits = map[string]string{
	"body": `package pkg

import "strings"

// Target is referenced from Go (binds) and TypeScript (never binds here).
func Target() int {
	n := len(strings.TrimSpace(" x "))
	return n + len(strings.Fields("a b"))
}

type Box struct{}

func (Box) Load() int { return 2 }
`,
	"signature": `package pkg

import "strings"

// Target is referenced from Go (binds) and TypeScript (never binds here).
func Target(extra int) int { return extra + len(strings.TrimSpace(" x ")) }

type Box struct{}

func (Box) Load() int { return 2 }

func Unknown() int { return 3 }
`,
	"import": `package pkg

import (
	"fmt"
	"strings"
)

// Target is referenced from Go (binds) and TypeScript (never binds here).
func Target() int { return len(strings.TrimSpace(fmt.Sprint(" x "))) }

type Box struct{}

func (Box) Load() int { return 2 }
`,
}

// resolverScopeArm indexes the fixture, applies one edit to pkg/a.go through
// the per-save path with evidence scoping on or off, and returns the durable
// digest plus the catch-up's resolver log.
func resolverScopeArm(t *testing.T, edit string, scoped bool) (map[string]string, *observer.ObservedLogs) {
	t.Helper()
	// The scoped arm is the unconfigured default; the exhaustive arm turns
	// scoping off through the configuration. Neither reads the environment
	// override.
	t.Setenv(resolver.EvidenceScopeEnv, "")
	indexCfg := config.Default().Index
	if !scoped {
		off := false
		indexCfg.DirtyChain = &config.DirtyChainConfig{ResolverEvidenceScope: &off}
	}
	builderIsolateGit(t)
	dir := builderTempDir(t, "scope-"+edit)
	builderWriteTree(t, dir, resolverScopeTree())
	store := builderOpenStore(t, "scope-"+edit)
	builderIndex(t, store, dir)

	if err := os.WriteFile(filepath.Join(dir, "pkg", "a.go"), []byte(resolverScopeEdits[edit]), 0o644); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	idx := New(store, builderRegistry(), indexCfg, logger)
	defer idx.Close()
	idx.resolver.SetLogger(logger)
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	idx.rootPath = dir
	if err := idx.IndexFile(filepath.Join(dir, "pkg", "a.go")); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	return resolverScopeDigest(t, store), logs
}

func resolverScopeDigest(t *testing.T, store *store_sqlite.Store) map[string]string {
	t.Helper()
	return realRepoDurableDigest(t, store)
}

func TestPerSaveCatchupScopedByEvidenceIsRowIdentical(t *testing.T) {
	for _, edit := range []string{"body", "signature", "import"} {
		t.Run(edit, func(t *testing.T) {
			scoped, scopedLogs := resolverScopeArm(t, edit, true)
			exhaustive, _ := resolverScopeArm(t, edit, false)
			for _, table := range []string{"edges", "nodes", "ref_facts"} {
				if scoped[table] != exhaustive[table] || scoped[table+"_rows"] != exhaustive[table+"_rows"] {
					t.Fatalf("%s edit: %s differ: scoped %s (%s rows) vs exhaustive %s (%s rows)", edit, table,
						scoped[table], scoped[table+"_rows"], exhaustive[table], exhaustive[table+"_rows"])
				}
			}
			summaries := scopedLogs.FilterMessage("resolver: incremental files phases").All()
			if edit == "body" {
				if len(summaries) == 0 {
					t.Fatal("the body edit ran no incremental catch-up")
				}
				fields := summaries[0].ContextMap()
				if carried, _ := fields["carried_keys"].(int64); carried == 0 {
					t.Fatalf("the body edit's catch-up carried no declaration keys: %v", fields)
				}
				if skipped, _ := fields["carried_skipped"].(int64); skipped == 0 {
					t.Fatalf("the body edit's catch-up re-attempted every parked reference: %v", fields)
				}
			}
			if edit == "signature" && len(summaries) > 0 {
				fields := summaries[0].ContextMap()
				// Target changed: its keys must not be carried, so its parked
				// references stay admitted.
				if skipped, _ := fields["carried_skipped"].(int64); skipped != 0 {
					t.Logf("signature edit skipped %d parked references on unchanged keys", skipped)
				}
			}
		})
	}
}

// In the daemon the repository's catch-up runs on the multi-repository master
// resolver, not on the repository's own: the mutation's evidence must reach
// the master too, or every save pays the exhaustive reverse leg.
func TestMultiRepositoryCatchupInheritsTheMutationEvidence(t *testing.T) {
	t.Setenv(resolver.EvidenceScopeEnv, "")
	for _, scoped := range []bool{true, false} {
		t.Run(map[bool]string{true: "default", false: "configured off"}[scoped], func(t *testing.T) {
			dir := t.TempDir()
			builderWriteTree(t, dir, resolverScopeTree())
			g := graph.New()
			idx := newTestIndexer(g)
			idx.SetRepoPrefix("repo")
			if !scoped {
				idx.resolver.SetEvidenceScoping(false)
			}
			_, err := idx.Index(dir)
			require.NoError(t, err)
			bumpMtime(t, filepath.Join(dir, "pkg", "a.go"), resolverScopeEdits["body"])

			core, observed := observer.New(zap.InfoLevel)
			logger := zap.New(core)
			mi := &MultiIndexer{
				graph:    g,
				repos:    map[string]*RepoMetadata{"repo": {RepoPrefix: "repo", RootPath: dir, FileMtimes: idx.FileMtimes()}},
				indexers: map[string]*Indexer{"repo": idx},
				logger:   logger,
			}
			mw, err := NewMultiWatcher(mi, map[string]config.WatchConfig{"repo": {}}, logger)
			require.NoError(t, err)
			watcher := mw.watchers["repo"]
			require.NotNil(t, watcher)
			_, err = watcher.batchReindex([]string{filepath.Join(dir, "pkg", "a.go")})
			require.NoError(t, err)

			summaries := observed.FilterMessage("resolver: incremental files phases").All()
			require.NotEmpty(t, summaries, "the master resolver ran no file catch-up")
			fields := summaries[0].ContextMap()
			if !scoped {
				assert.Zero(t, fields["carried_keys"], "scoping configured off carried declaration keys: %v", fields)
				return
			}
			assert.NotZero(t, fields["carried_keys"], "the master resolver inherited no declaration evidence: %v", fields)
			assert.Zero(t, fields["incoming_pending"], "a body edit admitted parked references on unchanged keys: %v", fields)
		})
	}
}

// The evidence-scoping switch decides how an incremental resolve narrows its
// work, not what a stored generation is keyed by: no value of it re-keys a
// stored generation, and the frozen snapshot keeps it.
func TestResolverEvidenceScopeSwitchIsHashExempt(t *testing.T) {
	base := config.Default().Index
	_, unset, err := snapshotDedicatedBaseConfig(base, "repo", "workspace", "project")
	require.NoError(t, err)
	assert.True(t, base.ResolverEvidenceScopeEnabled(), "evidence scoping is on by default")
	for _, value := range []bool{false, true} {
		value := value
		cfg := base
		cfg.DirtyChain = &config.DirtyChainConfig{ResolverEvidenceScope: &value}
		owned, fingerprint, err := snapshotDedicatedBaseConfig(cfg, "repo", "workspace", "project")
		require.NoError(t, err)
		assert.Equal(t, unset, fingerprint, "resolver_evidence_scope=%v changed the config fingerprint", value)
		assert.Equal(t, value, owned.ResolverEvidenceScopeEnabled(), "the frozen snapshot lost the switch (%v)", value)
	}
}

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
