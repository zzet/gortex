package indexer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractPinnedAcceptedGitBytesSurviveCacheEviction(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	runGit(t, f.root, "init")
	runGit(t, f.root, "config", "user.name", "Contract fixture")
	runGit(t, f.root, "config", "user.email", "contract-fixture@example.invalid")
	writeFile(t, filepath.Join(f.root, ".gitattributes"), "*.go text eol=crlf\n")
	runGit(t, f.root, "add", ".")
	runGit(t, f.root, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "commit", "-m", "canonical LF source")
	for path := range files {
		require.NoError(t, os.Remove(filepath.Join(f.root, path)))
	}
	runGit(t, f.root, "checkout-index", "--force", "--all")
	runGit(t, f.root, "diff", "--exit-code")
	raw, err := os.ReadFile(filepath.Join(f.root, "routes.go"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "\r\n")
	require.Equal(t, files["routes.go"], string(bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))))
	inputs, err := f.options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
	require.NoError(t, err)
	snapshot, _, err := NewContractFollowupCapture(f.options)(t.Context(), nil, inputs, "fixture", "")
	require.NoError(t, err)
	defer snapshot.Release()
	var file ContractFollowupFile
	for _, candidate := range snapshot.Files {
		if candidate.Path == "fixture/routes.go" {
			file = candidate
			break
		}
	}
	require.Equal(t, contractInputHash([]byte(files["routes.go"])), file.SourceFingerprint)
	warm, err := snapshot.ReadPinned(t.Context(), file, file.SourceFingerprint)
	require.NoError(t, err)
	require.Equal(t, []byte(files["routes.go"]), warm)
	// A real bounded-LRU eviction models the same missing optimization state as
	// a new process. Authoritative receipts/core rows and policy stay untouched.
	cache := f.mi.contractAcceptedSources()
	cache.mu.Lock()
	cache.limit = 1
	cache.mu.Unlock()
	cache.put(contractInputHash([]byte("!")), []byte("!"))
	_, cached := cache.get(file.SourceFingerprint)
	require.False(t, cached)
	recovered, err := snapshot.ReadPinned(t.Context(), file, file.SourceFingerprint)
	require.NoError(t, err, "Git's accepted LF bytes remain recoverable from a clean CRLF checkout without the transient cache")
	require.Equal(t, warm, recovered)
	require.Equal(t, file.SourceFingerprint, contractInputHash(recovered))
	_, err = f.job(nil, nil)
	require.NoError(t, err)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()), "full payload remains independently cold-equivalent")
	// A meaningful byte change cannot inherit the old accepted fingerprint.
	changed := bytes.ReplaceAll(raw, []byte("RoutePath"), []byte(`"/not-accepted"`))
	require.NotEqual(t, raw, changed)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "routes.go"), changed, 0o644))
	refused, err := snapshot.ReadPinned(t.Context(), file, file.SourceFingerprint)
	require.ErrorIs(t, err, errContractSourceMoved)
	require.Nil(t, refused)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "routes.go"), raw, 0o644))
	refused, err = snapshot.ReadPinned(t.Context(), file, contractInputHash([]byte("unrelated source")))
	require.ErrorIs(t, err, errContractSourceMoved)
	require.Nil(t, refused)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = snapshot.ReadPinned(cancelled, file, file.SourceFingerprint)
	require.ErrorIs(t, err, context.Canceled)
	// Once CRLF itself is accepted, preserve it exactly rather than imposing LF.
	current := strings.ReplaceAll(string(raw), "RoutePath", `"/accepted-crlf"`)
	f.save("routes.go", current)
	receipt, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", "fixture/routes.go")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, receipt.Accepted)
	require.Equal(t, contractInputHash([]byte(current)), receipt.SourceFingerprint)
	exact, err := snapshot.ReadPinned(t.Context(), file, receipt.SourceFingerprint)
	require.NoError(t, err)
	require.Equal(t, []byte(current), exact)
}
