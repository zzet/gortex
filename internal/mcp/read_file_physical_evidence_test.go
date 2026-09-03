package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadFilePhysicalEvidenceHashesFullDiskBuffer(t *testing.T) {
	srv, dir := setupTestServer(t)
	content := []byte("alpha\nbeta\ngamma\n")
	target := filepath.Join(dir, "evidence.txt")
	require.NoError(t, os.WriteFile(target, content, 0o644))

	result := callTool(t, srv, "read_file", map[string]any{
		"path":              "evidence.txt",
		"physical_evidence": true,
		"digest":            "sha256",
		"offset":            2,
		"limit":             1,
	})
	require.False(t, result.IsError)
	got := decodeFileOpsResult(t, result)
	sum := sha256.Sum256(content)

	require.Equal(t, "beta", got["content"])
	require.Equal(t, hex.EncodeToString(sum[:]), got["content_sha256"])
	require.Equal(t, "sha256", got["hash_algorithm"])
	require.Equal(t, "full_file", got["hash_scope"])
	require.Equal(t, "disk", got["hash_source"])
	require.Equal(t, "disk", got["content_source"])
	require.Equal(t, true, got["disk_verified"])
	require.Equal(t, false, got["same_buffer_as_content"])
	require.NotContains(t, got, "content_truncated")
	require.Equal(t, float64(len(content)), got["byte_count"])
	resolvedTarget, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	require.Equal(t, resolvedTarget, got["resolved_path"])
	require.Equal(t, "regular", got["file_kind"])
	require.NotEmpty(t, got["read_at"])
}

func TestReadFilePhysicalEvidenceSameBufferForFullText(t *testing.T) {
	srv, dir := setupTestServer(t)
	content := []byte("unchanged full text\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "full.txt"), content, 0o644))

	spec, ok := srv.facades.operation("read", "file")
	require.True(t, ok)
	arguments := normalizeFacadeArguments(spec, map[string]any{
		"operation": "file",
		"target":    map[string]any{"file": "full.txt"},
		"options": map[string]any{
			"physical_evidence": true,
			"digest":            "sha256",
		},
	})
	result := callTool(t, srv, spec.Legacy, arguments)
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)
	require.Equal(t, string(content), got["content"])
	require.Equal(t, true, got["same_buffer_as_content"])
	require.NotContains(t, got, "content_truncated")
}

func TestReadFilePhysicalEvidenceBinaryAndEmptyDigests(t *testing.T) {
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{name: "binary", content: []byte{0x00, 0xff, 0x10, 0x80}},
		{name: "empty", content: []byte{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, dir := setupTestServer(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"), test.content, 0o644))
			result := callTool(t, srv, "read_file", map[string]any{
				"path":              "blob.bin",
				"physical_evidence": true,
			})
			require.False(t, result.IsError)
			got := decodeFileOpsResult(t, result)
			sum := sha256.Sum256(test.content)
			require.Equal(t, hex.EncodeToString(sum[:]), got["content_sha256"])
			require.Equal(t, float64(len(test.content)), got["byte_count"])
			if test.name == "binary" {
				require.Equal(t, false, got["same_buffer_as_content"])
				require.NotContains(t, got, "content_truncated")
			}
		})
	}
}

func TestReadFilePhysicalEvidenceMaxCharsRetainsTruncationContract(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bounded.txt"), []byte("abcdef"), 0o644))
	got := decodeFileOpsResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":              "bounded.txt",
		"physical_evidence": true,
		"max_chars":         3,
	}))
	require.Equal(t, "abc", got["content"])
	require.Equal(t, true, got["content_truncated"])
	require.Equal(t, float64(3), got["max_chars"])
	require.Equal(t, false, got["same_buffer_as_content"])
}

func TestReadFilePhysicalEvidenceETagIgnoresObservationTime(t *testing.T) {
	srv, dir := setupTestServer(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stable.txt"), []byte("stable\n"), 0o644))
	args := map[string]any{"path": "stable.txt", "physical_evidence": true}

	first := decodeFileOpsResult(t, callTool(t, srv, "read_file", args))
	second := decodeFileOpsResult(t, callTool(t, srv, "read_file", args))
	require.Equal(t, first["etag"], second["etag"])
}

func TestReadFilePhysicalEvidenceRejectsInvalidDigestContract(t *testing.T) {
	srv, _ := setupTestServer(t)

	for _, args := range []map[string]any{
		{"path": "main.go", "physical_evidence": true, "digest": "md5"},
		{"path": "main.go", "digest": "sha256"},
	} {
		result := callTool(t, srv, "read_file", args)
		require.True(t, result.IsError)
	}
}

func TestReadFilePhysicalEvidenceRejectsSameSizeDriftDuringObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "drift.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))
	before, err := os.Stat(path)
	require.NoError(t, err)
	_, _, err = readPhysicalFileEvidenceObserved(path, func() {
		require.NoError(t, os.WriteFile(path, []byte("after!"), 0o644))
		require.NoError(t, os.Chtimes(path, before.ModTime(), before.ModTime()))
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "changed while it was being read")
}

func TestReadFilePhysicalEvidenceRejectsPathReplacementDuringObservation(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("replacing an open file is not reliably available on Windows CI")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "observed.txt")
	replacement := filepath.Join(dir, "replacement.txt")
	require.NoError(t, os.WriteFile(path, []byte("first"), 0o644))
	require.NoError(t, os.WriteFile(replacement, []byte("other"), 0o644))
	_, _, err := readPhysicalFileEvidenceObserved(path, func() {
		require.NoError(t, os.Rename(replacement, path))
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "changed while it was being read")
}

func TestReadFilePhysicalEvidenceReportsRequestedFileSymlink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	require.NoError(t, os.WriteFile(target, []byte("target"), 0o644))
	require.NoError(t, os.Symlink(target, link))
	content, evidence, err := readPhysicalFileEvidence(link)
	require.NoError(t, err)
	require.Equal(t, []byte("target"), content)
	require.True(t, evidence.symlinkResolved)
	resolved, err := filepath.EvalSymlinks(link)
	require.NoError(t, err)
	require.Equal(t, resolved, evidence.resolvedPath)
}

func TestReadFilePhysicalEvidenceAncestorSymlinkDoesNotMarkFileAsLink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	linkedDir := filepath.Join(dir, "linked")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	require.NoError(t, os.Symlink(realDir, linkedDir))
	path := filepath.Join(realDir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("content"), 0o644))
	_, evidence, err := readPhysicalFileEvidence(filepath.Join(linkedDir, "file.txt"))
	require.NoError(t, err)
	require.False(t, evidence.symlinkResolved)
	require.True(t, strings.HasSuffix(filepath.ToSlash(evidence.resolvedPath), "/real/file.txt"))
}

func TestReadFilePhysicalEvidenceRejectsOutAndBackSymlinkTarget(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	srv, repo := setupTestServer(t)
	inside := filepath.Join(repo, "inside.txt")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	link := filepath.Join(repo, "evidence-link.txt")
	require.NoError(t, os.WriteFile(inside, []byte("inside"), 0o644))
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o644))
	require.NoError(t, os.Symlink(inside, link))
	require.NoError(t, srv.guardSymlinkWithinRepo(context.Background(), link))

	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(outside, link))
	content, evidence, err := readPhysicalFileEvidence(link)
	require.NoError(t, err)
	require.Equal(t, []byte("outside"), content)

	require.NoError(t, os.Remove(link))
	require.NoError(t, os.Symlink(inside, link))
	require.NoError(t, srv.guardSymlinkWithinRepo(context.Background(), link), "a fresh post-read resolution alone sees only the restored inside target")
	err = srv.guardResolvedPathWithinRepo(context.Background(), link, evidence.resolvedPath)
	require.Error(t, err)
	require.ErrorIs(t, err, errPathEscape)
}

func TestReadFilePhysicalEvidenceRequiresSecretIntentForDigest(t *testing.T) {
	srv, dir := setupTestServer(t)
	content := []byte("PASSWORD=hunter2\n")
	path := filepath.Join(dir, ".env")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])

	refused := callTool(t, srv, "read_file", map[string]any{
		"path": ".env", "physical_evidence": true,
	})
	require.True(t, refused.IsError)
	require.Contains(t, toolResultText(refused), "requires allow_secrets=true")
	require.NotContains(t, toolResultText(refused), digest)

	allowed := callTool(t, srv, "read_file", map[string]any{
		"path": ".env", "physical_evidence": true, "allow_secrets": true,
	})
	require.False(t, allowed.IsError, toolResultText(allowed))
	got := decodeFileOpsResult(t, allowed)
	require.Equal(t, digest, got["content_sha256"])
	require.Equal(t, string(content), got["content"])
}

func TestReadFilePhysicalEvidenceInvalidUTF8IsNotSameWireBuffer(t *testing.T) {
	srv, dir := setupTestServer(t)
	content := []byte{0xff, 0xfe, 'x'}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "invalid.txt"), content, 0o644))

	result := callTool(t, srv, "read_file", map[string]any{
		"path": "invalid.txt", "physical_evidence": true,
	})
	require.False(t, result.IsError, toolResultText(result))
	got := decodeFileOpsResult(t, result)
	sum := sha256.Sum256(content)
	require.Equal(t, hex.EncodeToString(sum[:]), got["content_sha256"])
	require.Equal(t, false, got["same_buffer_as_content"])
}

func TestReadFilePhysicalEvidenceIsPublishedByFacadeSchema(t *testing.T) {
	srv, _ := setupTestServer(t)
	spec, ok := srv.facades.operation("read", "file")
	require.True(t, ok)
	capability := srv.facadeCapability(spec, true)
	schema := capability["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	options := properties["options"].(map[string]any)
	fields := options["properties"].(map[string]any)
	require.Contains(t, fields, "physical_evidence")
	require.Contains(t, fields, "digest")
}
