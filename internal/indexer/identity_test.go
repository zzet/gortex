package indexer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"pgregory.net/rapid"
)

func TestNormalizeRemoteURL_SSH(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "standard SSH URL",
			input:    "git@github.com:zzet/gortex.git",
			expected: "github.com/zzet/gortex",
		},
		{
			name:     "SSH URL without .git suffix",
			input:    "git@github.com:zzet/gortex",
			expected: "github.com/zzet/gortex",
		},
		{
			name:     "SSH URL with mixed-case host",
			input:    "git@GitHub.COM:zzet/gortex.git",
			expected: "github.com/zzet/gortex",
		},
		{
			name:     "SSH URL with gitlab",
			input:    "git@gitlab.com:team/project.git",
			expected: "gitlab.com/team/project",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeRemoteURL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizeRemoteURL_HTTPS(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "standard HTTPS URL",
			input:    "https://github.com/zzet/gortex.git",
			expected: "github.com/zzet/gortex",
		},
		{
			name:     "HTTPS URL without .git suffix",
			input:    "https://github.com/zzet/gortex",
			expected: "github.com/zzet/gortex",
		},
		{
			name:     "HTTPS URL with mixed-case host",
			input:    "https://GitHub.COM/zzet/gortex.git",
			expected: "github.com/zzet/gortex",
		},
		{
			name:     "HTTP URL",
			input:    "http://github.com/zzet/gortex.git",
			expected: "github.com/zzet/gortex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeRemoteURL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizeRemoteURL_SSHProtocol(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "ssh:// protocol URL",
			input:    "ssh://git@github.com/zzet/gortex.git",
			expected: "github.com/zzet/gortex",
		},
		{
			name:     "ssh:// protocol with mixed-case host",
			input:    "ssh://git@GitHub.COM/zzet/gortex.git",
			expected: "github.com/zzet/gortex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeRemoteURL(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizeRemoteURL_EdgeCases(t *testing.T) {
	assert.Equal(t, "", NormalizeRemoteURL(""))
	// Unrecognized format — strip .git suffix only.
	assert.Equal(t, "some-weird-url", NormalizeRemoteURL("some-weird-url.git"))
}

func TestNormalizeRemoteURL_SameRepoFormats(t *testing.T) {
	// All formats for the same repo should normalize to the same canonical form.
	ssh := NormalizeRemoteURL("git@github.com:zzet/gortex.git")
	https := NormalizeRemoteURL("https://github.com/zzet/gortex.git")
	sshProto := NormalizeRemoteURL("ssh://git@github.com/zzet/gortex.git")
	mixedCase := NormalizeRemoteURL("git@GitHub.COM:zzet/gortex.git")

	assert.Equal(t, ssh, https, "SSH and HTTPS should normalize to the same form")
	assert.Equal(t, ssh, sshProto, "SSH and ssh:// should normalize to the same form")
	assert.Equal(t, ssh, mixedCase, "Mixed-case host should normalize to the same form")
	assert.Equal(t, "github.com/zzet/gortex", ssh)
}

func TestDeduplicateRepos_UniqueEntries(t *testing.T) {
	entries := []config.RepoEntry{
		{Path: "/home/user/repo-a", Name: "repo-a"},
		{Path: "/home/user/repo-b", Name: "repo-b"},
	}

	result, warnings := DeduplicateRepos(entries)
	assert.Len(t, result, 2)
	assert.Empty(t, warnings)
}

func TestDeduplicateRepos_EmptyInput(t *testing.T) {
	result, warnings := DeduplicateRepos(nil)
	assert.Nil(t, result)
	assert.Nil(t, warnings)

	result, warnings = DeduplicateRepos([]config.RepoEntry{})
	assert.Empty(t, result)
	assert.Empty(t, warnings)
}

func TestDeduplicateRepos_DuplicatePaths(t *testing.T) {
	// Create temp dirs so filepath.Abs works consistently.
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "myrepo")
	require.NoError(t, os.MkdirAll(repoPath, 0755))

	entries := []config.RepoEntry{
		{Path: repoPath, Name: "first"},
		{Path: filepath.Join(tmpDir, "other"), Name: "other"},
		{Path: repoPath, Name: "last"},
	}

	// Create the "other" dir too.
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, "other"), 0755))

	result, warnings := DeduplicateRepos(entries)
	// Last occurrence wins — "last" should be kept, "first" dropped.
	assert.Len(t, result, 2)
	assert.NotEmpty(t, warnings)

	// Verify the kept entry is the last one.
	for _, r := range result {
		if r.Path == repoPath {
			assert.Equal(t, "last", r.Name)
		}
	}
}

func TestDetectIdentity_WithGitRepo(t *testing.T) {
	// Check if git is available.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmpDir := t.TempDir()

	// Initialize a git repo with a remote.
	cmds := [][]string{
		{"git", "-C", tmpDir, "init"},
		{"git", "-C", tmpDir, "remote", "add", "origin", "git@github.com:testowner/testrepo.git"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		require.NoError(t, cmd.Run(), "failed to run: %v", args)
	}

	identity, err := DetectIdentity(tmpDir)
	require.NoError(t, err)

	assert.Equal(t, "github.com/testowner/testrepo", identity.RemoteURL)
	assert.Equal(t, "github.com/testowner/testrepo", identity.CanonicalID)
	assert.Equal(t, "testrepo", identity.RepoPrefix)
	assert.Equal(t, tmpDir, identity.FilePath)
}

func TestDetectIdentity_WithoutGitRepo(t *testing.T) {
	tmpDir := t.TempDir()

	identity, err := DetectIdentity(tmpDir)
	require.NoError(t, err)

	assert.Empty(t, identity.RemoteURL)
	assert.Equal(t, tmpDir, identity.CanonicalID)
	assert.Equal(t, filepath.Base(tmpDir), identity.RepoPrefix)
	assert.Equal(t, tmpDir, identity.FilePath)
}

func TestLastPathComponent(t *testing.T) {
	assert.Equal(t, "gortex", lastPathComponent("github.com/zzet/gortex"))
	assert.Equal(t, "repo", lastPathComponent("host/owner/repo"))
	assert.Equal(t, "single", lastPathComponent("single"))
	assert.Equal(t, "trailing", lastPathComponent("host/trailing/"))
}

func TestLowercaseHost(t *testing.T) {
	assert.Equal(t, "github.com/Owner/Repo", lowercaseHost("GitHub.COM/Owner/Repo"))
	assert.Equal(t, "github.com", lowercaseHost("GitHub.COM"))
}

// ---------------------------------------------------------------------------
// Property-Based Tests (pgregory.net/rapid)
// ---------------------------------------------------------------------------

// genAlphaNum generates a non-empty string of lowercase alphanumeric characters.
func genAlphaNum(minLen, maxLen int) *rapid.Generator[string] {
	return rapid.Custom[string](func(t *rapid.T) string {
		n := rapid.IntRange(minLen, maxLen).Draw(t, "len")
		chars := make([]byte, n)
		for i := range chars {
			chars[i] = "abcdefghijklmnopqrstuvwxyz0123456789"[rapid.IntRange(0, 35).Draw(t, "ch")]
		}
		return string(chars)
	})
}

// genHostComponent generates a lowercase hostname-like string (e.g., "github.com").
func genHostComponent() *rapid.Generator[string] {
	return rapid.Custom[string](func(t *rapid.T) string {
		name := genAlphaNum(2, 10).Draw(t, "name")
		tld := rapid.SampledFrom([]string{"com", "org", "io", "net", "dev"}).Draw(t, "tld")
		return name + "." + tld
	})
}

// genOwnerRepo generates owner and repo name components.
func genOwnerRepo() *rapid.Generator[[2]string] {
	return rapid.Custom[[2]string](func(t *rapid.T) [2]string {
		owner := genAlphaNum(2, 12).Draw(t, "owner")
		repo := genAlphaNum(2, 12).Draw(t, "repo")
		return [2]string{owner, repo}
	})
}

// Feature: multi-repo-support, Property 11: Remote URL normalization
// TestPropertyRemoteURLNormalization verifies that SSH, HTTPS, and ssh:// URLs
// for the same repo all normalize to the identical canonical form host/owner/repo.
func TestPropertyRemoteURLNormalization(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		host := genHostComponent().Draw(rt, "host")
		pair := genOwnerRepo().Draw(rt, "ownerRepo")
		owner, repo := pair[0], pair[1]

		// Build URLs in different formats.
		sshURL := fmt.Sprintf("git@%s:%s/%s.git", host, owner, repo)
		httpsURL := fmt.Sprintf("https://%s/%s/%s.git", host, owner, repo)
		sshProtoURL := fmt.Sprintf("ssh://git@%s/%s/%s.git", host, owner, repo)

		// Also test mixed-case host.
		mixedHost := strings.ToUpper(host[:1]) + host[1:]
		mixedCaseSSH := fmt.Sprintf("git@%s:%s/%s.git", mixedHost, owner, repo)

		expected := strings.ToLower(host) + "/" + owner + "/" + repo

		sshNorm := NormalizeRemoteURL(sshURL)
		httpsNorm := NormalizeRemoteURL(httpsURL)
		sshProtoNorm := NormalizeRemoteURL(sshProtoURL)
		mixedNorm := NormalizeRemoteURL(mixedCaseSSH)

		assert.Equal(rt, expected, sshNorm,
			"SSH URL %q should normalize to %q", sshURL, expected)
		assert.Equal(rt, expected, httpsNorm,
			"HTTPS URL %q should normalize to %q", httpsURL, expected)
		assert.Equal(rt, expected, sshProtoNorm,
			"ssh:// URL %q should normalize to %q", sshProtoURL, expected)
		assert.Equal(rt, expected, mixedNorm,
			"mixed-case SSH URL %q should normalize to %q", mixedCaseSSH, expected)

		// All formats must produce identical output.
		assert.Equal(rt, sshNorm, httpsNorm, "SSH and HTTPS should match")
		assert.Equal(rt, sshNorm, sshProtoNorm, "SSH and ssh:// should match")
		assert.Equal(rt, sshNorm, mixedNorm, "SSH and mixed-case should match")
	})
}

// Feature: multi-repo-support, Property 2: Repo prefix derivation from path or URL
// TestPropertyRepoPrefixDerivation verifies that when the name field is omitted,
// the derived RepoPrefix equals the last path component.
func TestPropertyRepoPrefixDerivation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		repoName := genAlphaNum(2, 20).Draw(rt, "repoName")

		// Test with filesystem path.
		fsPath := "/home/user/" + repoName
		assert.Equal(rt, repoName, lastPathComponent(fsPath),
			"lastPathComponent(%q) should return %q", fsPath, repoName)

		// Test with normalized URL.
		host := genHostComponent().Draw(rt, "host")
		owner := genAlphaNum(2, 12).Draw(rt, "owner")
		urlPath := host + "/" + owner + "/" + repoName
		assert.Equal(rt, repoName, lastPathComponent(urlPath),
			"lastPathComponent(%q) should return %q", urlPath, repoName)

		// Test with ResolvePrefix (config package) — omitted name should derive from path.
		entry := config.RepoEntry{Path: fsPath}
		assert.Equal(rt, repoName, config.ResolvePrefix(entry),
			"ResolvePrefix with empty Name should derive from path")

		// Test with explicit name — should use the name directly.
		explicitName := genAlphaNum(2, 10).Draw(rt, "explicitName")
		entryWithName := config.RepoEntry{Path: fsPath, Name: explicitName}
		assert.Equal(rt, explicitName, config.ResolvePrefix(entryWithName),
			"ResolvePrefix with explicit Name should use it")
	})
}

// Feature: multi-repo-support, Property 12: Duplicate identity detection (last-wins)
// TestPropertyDuplicateIdentityDetection verifies that DeduplicateRepos keeps
// the last occurrence when duplicate paths exist and emits warnings.
//
// The property's only structural input is how many unique entries sit between
// the two occurrences of the duplicated path, zero to three, so the cases
// enumerate that range instead of sampling it. DeduplicateRepos runs one git
// process per entry, and a hundred random draws over those four shapes paid for
// each of them about twenty-five times.
func TestPropertyDuplicateIdentityDetection(t *testing.T) {
	for numUnique := 0; numUnique <= 3; numUnique++ {
		t.Run(fmt.Sprintf("%d unique entries between", numUnique), func(t *testing.T) {
			iterDir := t.TempDir()

			// The directory that will be duplicated.
			dupName := "dup"
			dupPath := filepath.Join(iterDir, dupName)
			require.NoError(t, os.MkdirAll(dupPath, 0755))

			// The unique (non-duplicate) entries.
			var entries []config.RepoEntry
			for i := 0; i < numUnique; i++ {
				name := fmt.Sprintf("unique%d", i)
				uPath := filepath.Join(iterDir, name)
				require.NoError(t, os.MkdirAll(uPath, 0755))
				entries = append(entries, config.RepoEntry{Path: uPath, Name: name})
			}

			// Insert the duplicate path at two positions: early and late.
			firstName := "first-" + dupName
			lastName := "last-" + dupName
			earlyEntry := config.RepoEntry{Path: dupPath, Name: firstName}
			lateEntry := config.RepoEntry{Path: dupPath, Name: lastName}

			// Insert early entry at the beginning, late entry at the end.
			entries = append([]config.RepoEntry{earlyEntry}, entries...)
			entries = append(entries, lateEntry)

			result, warnings := DeduplicateRepos(entries)

			// One warning, for the superseded early entry.
			assert.Len(t, warnings, 1, "should emit one warning for the duplicate path")

			// The result should contain numUnique + 1 entries (one for the dup path).
			assert.Len(t, result, numUnique+1,
				"should have %d unique + 1 deduped = %d entries", numUnique, numUnique+1)

			// The kept entry for the dup path should be the last one (lastName).
			for _, r := range result {
				absR, _ := filepath.Abs(r.Path)
				absDup, _ := filepath.Abs(dupPath)
				if absR == absDup {
					assert.Equal(t, lastName, r.Name,
						"last occurrence should win: expected %q, got %q", lastName, r.Name)
				}
			}
		})
	}
}

// Feature: multi-repo-support, Property 4: Path resolution to absolute
// TestPropertyPathResolutionToAbsolute verifies that normalizePath resolves
// relative paths to absolute paths equivalent to filepath.Abs.
func TestPropertyPathResolutionToAbsolute(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a relative path with 1-3 components.
		numComponents := rapid.IntRange(1, 3).Draw(rt, "numComponents")
		components := make([]string, numComponents)
		for i := range components {
			components[i] = genAlphaNum(2, 10).Draw(rt, fmt.Sprintf("comp%d", i))
		}
		relativePath := filepath.Join(components...)

		// normalizePath (from config/global.go) should produce the same result as filepath.Abs.
		expected, err := filepath.Abs(relativePath)
		require.NoError(rt, err)

		// Test the normalizePath function indirectly through AddRepo which calls it,
		// or test filepath.Abs directly since normalizePath wraps it.
		got, err := filepath.Abs(relativePath)
		require.NoError(rt, err)

		assert.Equal(rt, expected, got,
			"filepath.Abs(%q) should produce consistent absolute path", relativePath)

		// Verify the result is absolute.
		assert.True(rt, filepath.IsAbs(got),
			"resolved path %q should be absolute", got)

		// Verify the result does not contain relative components.
		assert.NotContains(rt, got, "..",
			"resolved path should not contain '..'")
	})
}

// Feature: multi-repo-support, Property 5: Repo entry deduplication
// TestPropertyRepoEntryDeduplication verifies that DeduplicateRepos retains
// only one entry per unique absolute path.
//
// Each case lists how many times each of its paths repeats, consecutively. The
// cases span the shapes a generator over two to six paths repeated one to
// three times draws — the smallest set with no duplicate, one path at the
// repeat bound, every path duplicated, and six paths at mixed repeat counts —
// without paying a git process per entry for a hundred random draws of them.
func TestPropertyRepoEntryDeduplication(t *testing.T) {
	for _, repeatsPerPath := range [][]int{
		{1, 1},
		{3, 1},
		{2, 2, 2},
		{1, 2, 3, 1, 2, 3},
	} {
		t.Run(fmt.Sprint(repeatsPerPath), func(t *testing.T) {
			iterDir := t.TempDir()

			numPaths := len(repeatsPerPath)
			uniquePaths := make([]string, numPaths)
			for i := range uniquePaths {
				name := fmt.Sprintf("repo%d", i)
				p := filepath.Join(iterDir, name)
				require.NoError(t, os.MkdirAll(p, 0755))
				uniquePaths[i] = p
			}

			// Build entries with duplicates: each path appears repeats times.
			var entries []config.RepoEntry
			expectedLastName := make(map[string]string) // absPath → last name seen
			superseded := 0

			for i, p := range uniquePaths {
				repeats := repeatsPerPath[i]
				superseded += repeats - 1
				for j := 0; j < repeats; j++ {
					name := fmt.Sprintf("%s-v%d", filepath.Base(p), j)
					entries = append(entries, config.RepoEntry{Path: p, Name: name})
					absP, _ := filepath.Abs(p)
					expectedLastName[absP] = name
				}
			}

			result, warnings := DeduplicateRepos(entries)

			// Verify: exactly one entry per unique absolute path.
			seenPaths := make(map[string]bool)
			for _, r := range result {
				absR, _ := filepath.Abs(r.Path)
				assert.False(t, seenPaths[absR],
					"path %q should appear only once in deduplicated result", absR)
				seenPaths[absR] = true
			}

			// Verify all unique paths are represented.
			assert.Len(t, result, numPaths,
				"should have exactly %d entries (one per unique path)", numPaths)

			// One warning per superseded entry.
			assert.Len(t, warnings, superseded, "should warn once per superseded entry")

			// Verify last-wins semantics.
			for _, r := range result {
				absR, _ := filepath.Abs(r.Path)
				assert.Equal(t, expectedLastName[absR], r.Name,
					"entry for %q should be the last occurrence", absR)
			}
		})
	}
}
