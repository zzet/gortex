package store_sqlite

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Keep the original wide-candidate implementation as an independent result
// oracle, including metadata decoding of candidates that are not seeds.
func contractSeedNodesWideReference(t *testing.T, s *Store, repo string) ([]*graph.Node, error) {
	t.Helper()
	kinds, _ := json.Marshal([]string{string(graph.KindField), string(graph.KindType), string(graph.KindInterface), string(graph.KindMethod)})
	rows, err := s.db.QueryContext(t.Context(), repoNodesByKindsWithMetaKeyQuery(false), string(kinds), repo, s.viewGen, s.viewGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*graph.Node
	seen := map[string]bool{}
	for rows.Next() {
		n, err := scanNodeCursor(rows)
		if err != nil {
			return nil, err
		}
		if n == nil || seen[n.ID] {
			continue
		}
		_, spring := n.Meta["spring_config_keys"]
		if spring || n.Kind == graph.KindMethod && n.Language == "java" {
			seen[n.ID] = true
			out = append(out, n)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b *graph.Node) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out, nil
}

func TestContractSeedNodeCandidatesKeepFullRowsAndExactIdentities(t *testing.T) {
	s := openPayloadStore(t)
	var nodes []*graph.Node
	for i := 0; i < 430; i++ {
		lang := "go"
		if i < 129 {
			lang = "java"
		}
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("repo/f.java::M%03d", i), Name: fmt.Sprintf("M%03d", i), QualName: fmt.Sprintf("pkg.M%03d", i), Kind: graph.KindMethod, RepoPrefix: "repo", WorkspaceID: "w", ProjectID: "p", FilePath: "repo/f.java", Language: lang, StartLine: i + 1, EndLine: i + 2, StartColumn: 3, EndColumn: 7, Meta: map[string]any{"signature": "full signature", "doc_comment": "full rich doc", "nested": map[string]any{"kept": []string{"a", "b"}}}})
	}
	nodes = append(nodes, &graph.Node{ID: "repo/raw\xff", Kind: graph.KindField, RepoPrefix: "repo", Language: "go", FilePath: "repo/f.go", Meta: map[string]any{"spring_config_keys": nil, "kept": true}}, &graph.Node{ID: "other/f.java::M", Kind: graph.KindMethod, RepoPrefix: "other", Language: "java"}, &graph.Node{ID: "repo/not-a-seed", Kind: graph.KindConfigKey, RepoPrefix: "repo", Meta: map[string]any{"spring_config_keys": "outside kinds"}})
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	// The ordinary writer normalizes invalid UTF8 while serializing its rows.
	// Materialize an actual legacy SQLite TEXT identity before testing exact binds.
	_, legacyErr := s.writerDB.Exec(`UPDATE nodes SET id=CAST(? AS TEXT) WHERE view_gen=0 AND repo_prefix='repo' AND kind='field'`, []byte("repo/raw\xff"))
	require.NoError(t, legacyErr)
	want, err := contractSeedNodesWideReference(t, s, "repo")
	require.NoError(t, err)
	require.Len(t, want, 130)
	got, err := s.ContractSeedNodesContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Equal(t, want, got, "all rich columns/promoted/nested metadata and raw identity bytes stay exact across128-row pages")
	require.True(t, slices.ContainsFunc(got, func(n *graph.Node) bool { return n.ID == "repo/raw\xff" }))
}

func TestContractSeedNodeCandidatesKeepLegacyMetadataAndGenerationScope(t *testing.T) {
	s := openPayloadStore(t)
	nodes := []*graph.Node{
		{ID: "standalone", Kind: graph.KindField, Meta: map[string]any{"spring_config_keys": "empty repo"}},
		{ID: "repo/json", Kind: graph.KindType, RepoPrefix: "repo", Meta: map[string]any{"spring_config_keys": "json"}},
		{ID: "repo/gob", Kind: graph.KindInterface, RepoPrefix: "repo", Meta: map[string]any{"spring_config_keys": "gob"}},
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	var legacy bytes.Buffer
	require.NoError(t, gob.NewEncoder(&legacy).Encode(map[string]any{"spring_config_keys": "gob", "kept": int64(3)}))
	_, err := s.writerDB.Exec(`UPDATE nodes SET meta=? WHERE id=? AND view_gen=0`, legacy.Bytes(), "repo/gob")
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta=? WHERE id=? AND view_gen=0`, []byte(`{"spring_config_\u006beys":"json","kept":[1,2]}`), "repo/json")
	require.NoError(t, err)
	for _, repo := range []string{"repo", "", "other"} {
		want, err := contractSeedNodesWideReference(t, s, repo)
		require.NoError(t, err)
		got, err := s.ContractSeedNodesContext(t.Context(), repo)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	seedPayloadControlPlane(t, s)
	_, payload, err := s.BeginPayloadGeneration(t.Context(), payloadRequest())
	require.NoError(t, err)
	require.NoError(t, payload.AddBatchChecked([]*graph.Node{{ID: "repo/upper.java", Kind: graph.KindMethod, RepoPrefix: "repo", Language: "java", Meta: map[string]any{"signature": "only upper"}}}, nil))
	got, err := payload.ContractSeedNodesContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "repo/upper.java", got[0].ID)
	lower, err := s.ContractSeedNodesContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Len(t, lower, 2)
}

func TestContractSeedNodeCandidatesUseOneSnapshotAcrossHydration(t *testing.T) {
	s := openPayloadStore(t)
	id := "repo/f.java::M"
	original := &graph.Node{ID: id, Kind: graph.KindMethod, Name: "M", RepoPrefix: "repo", Language: "java", Meta: map[string]any{"signature": "old snapshot"}}
	require.NoError(t, s.AddBatchChecked([]*graph.Node{original}, nil))
	want, err := contractSeedNodesWideReference(t, s, "repo")
	require.NoError(t, err)
	got, err := s.contractSeedNodesContext(t.Context(), "repo", func() {
		changed := *original
		changed.Language = "go"
		changed.Meta = map[string]any{"signature": "new nonseed"}
		require.NoError(t, s.AddBatchChecked([]*graph.Node{&changed}, nil))
	})
	require.NoError(t, err)
	require.Equal(t, want, got, "candidate membership must never hydrate a later row version")
	after, err := s.ContractSeedNodesContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Empty(t, after)
}

func TestContractSeedNodeCandidatesRefuseMalformedMetadataAndCancellation(t *testing.T) {
	for _, seed := range []bool{false, true} {
		t.Run(fmt.Sprint("bad_metadata_seed_", seed), func(t *testing.T) {
			s := openPayloadStore(t)
			lang := "go"
			if seed {
				lang = "java"
			}
			require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "repo/f", Kind: graph.KindMethod, RepoPrefix: "repo", Language: lang, Meta: map[string]any{"kept": true}}}, nil))
			_, err := s.writerDB.Exec(`UPDATE nodes SET meta=x'00ff' WHERE id='repo/f' AND view_gen=0`)
			require.NoError(t, err)
			_, err = contractSeedNodesWideReference(t, s, "repo")
			require.Error(t, err)
			got, err := s.ContractSeedNodesContext(t.Context(), "repo")
			require.Error(t, err)
			require.Nil(t, got, "a malformed nonseed is not a certified absence")
		})
	}
	s := openPayloadStore(t)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "repo/f", Kind: graph.KindMethod, RepoPrefix: "repo", Language: "java"}}, nil))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got, err := s.contractSeedNodesContext(ctx, "repo", cancel)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got)
	canceled, done := context.WithCancel(t.Context())
	done()
	_, err = s.ContractSeedNodesContext(canceled, "repo")
	require.ErrorIs(t, err, context.Canceled)
}

func TestContractSeedNodeCandidatesRetainOriginalRowLimit(t *testing.T) {
	s := openPayloadStore(t)
	nodes := make([]*graph.Node, graph.ContractProjectionRowLimit+1)
	for i := range nodes {
		nodes[i] = &graph.Node{ID: fmt.Sprintf("repo/M%d", i), Kind: graph.KindMethod, RepoPrefix: "repo", Language: "java"}
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	got, err := s.ContractSeedNodesContext(t.Context(), "repo")
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Nil(t, got)
}
