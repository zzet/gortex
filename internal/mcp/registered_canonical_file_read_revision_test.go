package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// An unrelated SQL write moves the store revision while a file read holds its
// checked rows. Both file metadata reads may answer only with definitions
// proven again from the request's unchanged canonical bytes, and must keep
// the refusal when those bytes, their checkout or their owner moved.
func TestRegisteredCanonicalFileReadAfterUnrelatedSQLRevision(t *testing.T) {
	for _, tool := range []string{"get_editing_context", "get_file_summary"} {
		for _, transition := range []string{"same_file", "same_stamp_byte_drift", "foreign_checkout_proof", "new_physical_owner"} {
			t.Run(tool+"/"+transition, func(t *testing.T) {
				testRegisteredCanonicalFileReadAfterUnrelatedSQLRevision(t, tool, transition)
			})
		}
	}
}

func testRegisteredCanonicalFileReadAfterUnrelatedSQLRevision(t *testing.T, tool, transition string) {
	t.Helper()
	selectedProjectionRegistry(t)
	s, sql, path := newAssemblyRetirementFixture(t)
	backend := &assemblyRetirementStore{Store: sql}
	s.graph = backend
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	t.Cleanup(func() {
		close(pending)
		s.mutationReceipts.Range(func(_, value any) bool {
			select {
			case <-value.(*mutationReceipt).done:
			case <-time.After(3 * time.Second):
				t.Error("owned mutation publication did not join")
			}
			return true
		})
	})
	root := filepath.Dir(path)
	revisionFile := "revision_witness.go"
	witness := filepath.Join(root, revisionFile)
	require.NoError(t, os.WriteFile(witness, []byte("package probe\n\nfunc RevisionWitness() {}\n"), 0600))
	require.NoError(t, s.indexer.IndexFile(witness))
	target := "main.go"
	prefix := s.indexer.RepoPrefix()
	before, err := sql.FileMetasByPaths(prefix, []string{target, revisionFile})
	require.NoError(t, err)
	require.NotEmpty(t, before[target].ContentHash)
	require.NotEmpty(t, before[revisionFile].ContentHash)
	physical, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Nil(t, liveEntry(pendingFileProjections, path))
	foreignRoot := t.TempDir()
	foreignPath := filepath.Join(foreignRoot, target)
	require.NoError(t, os.WriteFile(foreignPath, physical, 0600))
	foreignStamp, err := statDiskStamp(foreignPath)
	require.NoError(t, err)

	const session = "canonical-context-revision"
	require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { s.DisconnectSession(session) })
	s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	base := s.WithClientSession(WithSessionID(t.Context(), session), session)
	require.NotNil(t, s.MCPServer().HandleMessage(base, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"canonical-context-revision","version":"1"}}}`)))

	var owner func(func()) bool
	var deadline time.Time
	callbacks := 0
	backend.write = func() error { return sql.DeleteFileMetasByFiles(prefix, []string{revisionFile}) }
	backend.after = func(ctx context.Context, scopeErr error) {
		owner(func() {
			callbacks++
			require.ErrorIs(t, scopeErr, graph.ErrContractProjectionStale)
			require.Equal(t, deadline, ctx.Value(projectedSourceDeadlineKey{}))
			require.False(t, s.hasRequestProjectionFact(ctx, target))
			after, err := sql.FileMetasByPaths(prefix, []string{target, revisionFile})
			require.NoError(t, err)
			require.Equal(t, before[target], after[target])
			require.NotContains(t, after, revisionFile)
			current, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, physical, current)
			require.Nil(t, liveEntry(pendingFileProjections, path))

			switch transition {
			case "same_stamp_byte_drift":
				stamp, err := statDiskStamp(path)
				require.NoError(t, err)
				drift := strings.Replace(string(physical), "func Target()", "func Alterd()", 1)
				require.Len(t, drift, len(physical))
				require.NotEqual(t, string(physical), drift)
				require.NoError(t, os.WriteFile(path, []byte(drift), 0600))
				require.NoError(t, os.Chtimes(path, stamp.info.ModTime(), stamp.info.ModTime()))
				after, err := statDiskStamp(path)
				require.NoError(t, err)
				require.True(t, stamp.matches(after))
			case "foreign_checkout_proof":
				// Present the same bytes from an independent checkout to
				// the guarded fallback without changing the request's route.
				if proof, _ := ctx.Value(canonicalSummarySourceKey{}).(*canonicalSummarySource); proof != nil {
					require.Equal(t, physical, proof.bytes)
					proof.root, proof.abs, proof.key, proof.stamp = foreignRoot, foreignPath, pathkey.MutationKey(foreignPath), foreignStamp
				}
			case "new_physical_owner":
				held, release, err := acquireMutationPath(t.Context(), path)
				require.NoError(t, err)
				_, editErr := s.commitFileMutation(held, "write_file", "", "", target, path, physical, 0600)
				release()
				require.NoError(t, editErr)
				entry := liveEntry(pendingFileProjections, path)
				require.NotNil(t, entry)
				require.Positive(t, entry.commitSeq)
			}
			require.True(t, time.Now().Before(deadline), "controlled transition must fit the original allowance")
		})
	}
	backend.armed.Store(true)
	deadline = time.Now().Add(projectedReadBudget)
	caller := context.WithValue(base, projectedSourceDeadlineKey{}, deadline)
	result, err := canonicalReadWithOwner(t, func(onOwner func(func()) bool) (*mcplib.CallToolResult, error) {
		owner = onOwner
		operation := "editing_context"
		if tool == "get_file_summary" {
			operation = "summary"
		}
		return externalVersionRegisteredCall(s, caller, 20, "read", map[string]any{
			"operation": operation, "target": map[string]any{"file": target},
			"output": map[string]any{"format": "json", "max_bytes": 0},
		})
	})
	require.NoError(t, err)
	require.Equal(t, 1, callbacks)
	require.NotNil(t, result)
	if transition != "same_file" {
		require.True(t, result.IsError, "unproved canonical data must keep the refusal: %s", resultText(result))
		require.Contains(t, resultText(result), graph.ErrContractProjectionStale.Error())
		return
	}
	requireCanonicalDefinitionRows(t, tool, result, physical)
	requireCanonicalFileReadPartial(t, tool, result)
}

func requireCanonicalFileReadPartial(t *testing.T, tool string, result *mcplib.CallToolResult) {
	t.Helper()
	if tool == "get_file_summary" {
		requireCanonicalSummaryPartial(t, result)
		return
	}
	requireCanonicalEditingContextPartial(t, result)
}

func requireCanonicalEditingContextPartial(t *testing.T, result *mcplib.CallToolResult) {
	t.Helper()
	require.False(t, result.IsError, "unchanged, independently checked canonical file should return partial data: %s", resultText(result))
	fields := extractTextResult(t, result)
	require.NotEmpty(t, fields["file"])
	require.Contains(t, resultText(result), "main.go::Target")
	require.Contains(t, resultText(result), "main.go::Neighbor")
	require.Empty(t, fields["imports"])
	require.Empty(t, fields["calls"])
	require.Empty(t, fields["called_by"])
	fresh, ok := fields["freshness"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pending", fresh["graph_freshness"])
	require.Equal(t, "store_busy", fresh["reason"])
	require.Equal(t, false, fresh["complete"])
	require.NotEqual(t, true, fresh["readback_exact"])
}

// requireCanonicalSummaryPartial accepts only the definitions proven from the
// canonical bytes. The rejected SQL rows supply no edges or dependents.
func requireCanonicalSummaryPartial(t *testing.T, result *mcplib.CallToolResult) {
	t.Helper()
	require.False(t, result.IsError, "unchanged, independently checked canonical file should return partial data: %s", resultText(result))
	fields := extractTextResult(t, result)
	require.Contains(t, resultText(result), "main.go::Target")
	require.Contains(t, resultText(result), "main.go::Neighbor")
	require.Empty(t, fields["edges"])
	require.EqualValues(t, 0, fields["total_edges"])
	require.Empty(t, fields["dependents"])
	fresh, ok := fields["freshness"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pending", fresh["graph_freshness"])
	require.Equal(t, "store_busy", fresh["reason"])
	require.Equal(t, false, fresh["complete"])
	require.NotEqual(t, true, fresh["readback_exact"])
}
