package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractLinkedAcceptedBytesRemainPinnedAfterNextDiskSave(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "routes.go")
	// Checkout and ref construction use this same owner-installed builder.
	// The old sparse journal did not populate the MI cache capture reads.
	owner := &CheckoutLifecycle{mi: f.mi}
	require.NotNil(t, w.builder.contractCoreRuntime)
	require.NoError(t, owner.SetContractCoreRuntime(*w.builder.contractCoreRuntime))
	w.builder = owner.installContractCoreBuilder(w.builder)
	accepted := strings.ReplaceAll(jobRoutes, "RoutePath", `"/linked-cache-accepted"`)
	files["routes.go"] = accepted
	writeFile(t, filepath.Join(w.root, "routes.go"), accepted)
	parent := w.view(t)
	dirty := w.build(t, DirtyLayerGenerationKind, w.commit, parent, "routes.go")
	parent.Close()
	w.route(t, w.commit, dirty)
	view := w.view(t)
	report, err := w.job(t, view, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(w.contracts(t, view)))
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), view, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	snapshot, _, err := NewContractFollowupCapture(f.options)(t.Context(), view, inputs, "fixture", contractLinkedCheckout)
	require.NoError(t, err)
	defer snapshot.Release()
	var file ContractFollowupFile
	for _, row := range snapshot.Files {
		if row.Path == "fixture/routes.go" {
			file = row
		}
	}
	require.Equal(t, contractInputHash([]byte(accepted)), file.SourceFingerprint)
	before, err := snapshot.Scan(t.Context(), []string{file.Path})
	require.NoError(t, err)
	// A save overtakes the disk, not this independently leased accepted
	// generation. Exact source bytes remain an optimization, never authority.
	newer := strings.ReplaceAll(accepted, "/linked-cache-accepted", "/not-yet-accepted")
	require.NoError(t, os.WriteFile(filepath.Join(w.root, "routes.go"), []byte(newer), 0o644))
	after, err := snapshot.Scan(t.Context(), []string{file.Path})
	require.NoError(t, err)
	require.Equal(t, before.Stamps, after.Stamps, "the selected accepted receipt did not move with the disk save")
	pinned, err := snapshot.ReadPinned(t.Context(), file, file.SourceFingerprint)
	require.NoError(t, err, "sparse accepted bytes must be retained by the same bounded owner cache capture reads")
	require.Equal(t, []byte(accepted), pinned)
	// The existing bound is unchanged. Eviction must still refuse unavailable
	// old bytes rather than reading newer disk content under an old stamp.
	cache := f.mi.contractAcceptedSources()
	cache.mu.Lock()
	cache.limit = 1
	cache.mu.Unlock()
	cache.put(contractInputHash([]byte("!")), []byte("!"))
	_, present := cache.get(file.SourceFingerprint)
	require.False(t, present)
	pinned, err = snapshot.ReadPinned(t.Context(), file, file.SourceFingerprint)
	require.ErrorIs(t, err, errContractSourceMoved)
	require.Nil(t, pinned)
	require.NoError(t, os.WriteFile(filepath.Join(w.root, "routes.go"), []byte(accepted), 0o644))
	pinned, err = snapshot.ReadPinned(t.Context(), file, file.SourceFingerprint)
	require.NoError(t, err)
	require.Equal(t, []byte(accepted), pinned, "exact disk fallback remains valid after cache eviction")
}
