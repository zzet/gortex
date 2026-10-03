package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"pgregory.net/rapid"

	"github.com/zzet/gortex/internal/testenv"
)

// TestDefaultGlobalConfigPath_HonorsHomeChange guards the regression
// where DefaultGlobalConfigPath cached its result with sync.Once. Whichever
// caller fired first pinned the path for the rest of the process — any
// test that later set HOME via t.Setenv silently kept writing into the
// developer's real ~/.gortex/config.yaml. The function must
// re-resolve HOME on every call.
func TestDefaultGlobalConfigPath_HonorsHomeChange(t *testing.T) {
	// Pin XDG_CONFIG_HOME empty: when it is set in the ambient
	// environment (the GitHub ubuntu runner sets it), an absolute value
	// wins over $HOME/.config by design and would mask the HOME change
	// this test asserts.
	t.Setenv("XDG_CONFIG_HOME", "")
	homeA := t.TempDir()
	testenv.SetHome(t, homeA)
	gotA := DefaultGlobalConfigPath()
	wantA := filepath.Join(homeA, ".gortex", "config.yaml")
	if gotA != wantA {
		t.Fatalf("first call: got %s, want %s", gotA, wantA)
	}

	homeB := t.TempDir()
	testenv.SetHome(t, homeB)
	gotB := DefaultGlobalConfigPath()
	wantB := filepath.Join(homeB, ".gortex", "config.yaml")
	if gotB != wantB {
		t.Fatalf("after HOME change: got %s, want %s — path appears cached", gotB, wantB)
	}
}

// TestDefaultGlobalConfigPath_HonorsXDGConfigHome verifies the global
// config path is routed through the XDG resolver: an absolute
// $XDG_CONFIG_HOME relocates it, while an unset variable uses the
// unified $HOME/.gortex location.
func TestDefaultGlobalConfigPath_HonorsXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	testenv.SetHome(t, home)

	// Unset: historical default.
	t.Setenv("XDG_CONFIG_HOME", "")
	wantUnset := filepath.Join(home, ".gortex", "config.yaml")
	if got := DefaultGlobalConfigPath(); got != wantUnset {
		t.Fatalf("XDG_CONFIG_HOME unset: got %s, want %s", got, wantUnset)
	}

	// Set to an absolute path: honored.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	wantSet := filepath.Join(xdg, "gortex", "config.yaml")
	if got := DefaultGlobalConfigPath(); got != wantSet {
		t.Fatalf("XDG_CONFIG_HOME set: got %s, want %s", got, wantSet)
	}

	// Relative value: ignored per the XDG spec, fall back to default.
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	if got := DefaultGlobalConfigPath(); got != wantUnset {
		t.Fatalf("relative XDG_CONFIG_HOME: got %s, want %s (must be ignored)", got, wantUnset)
	}
}

func TestLoadGlobal_FileNotExist(t *testing.T) {
	gc, err := LoadGlobal("/tmp/nonexistent-gortex-test/config.yaml")
	require.NoError(t, err)
	assert.NotNil(t, gc)
	assert.Empty(t, gc.Repos)
	assert.Empty(t, gc.Projects)
	assert.Empty(t, gc.ActiveProject)
}

func TestLoadGlobal_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	content := `
active_project: my-project
repos:
  - path: /home/user/repo1
    name: repo1
  - path: /home/user/repo2
projects:
  my-project:
    repos:
      - path: /home/user/repo1
        name: repo1
        ref: work
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	gc, err := LoadGlobal(path)
	require.NoError(t, err)
	assert.Equal(t, "my-project", gc.ActiveProject)
	assert.Len(t, gc.Repos, 2)
	assert.Equal(t, "repo1", gc.Repos[0].Name)
	assert.Equal(t, "/home/user/repo2", gc.Repos[1].Path)
	assert.Empty(t, gc.Repos[1].Name)
	assert.Len(t, gc.Projects, 1)
	proj := gc.Projects["my-project"]
	assert.Len(t, proj.Repos, 1)
	assert.Equal(t, "work", proj.Repos[0].Ref)
}

func TestLoadGlobal_DaemonHTTPAddr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("daemon:\n  http_addr: 127.0.0.1:7411\n"), 0644))

	gc, err := LoadGlobal(path)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7411", gc.Daemon.HTTPAddr)
}

func TestUnknownGlobalKeysRecognizesDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("daemon:\n  memory_limit: 4GiB\ntypo: true\n")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	require.Equal(t, []string{"typo"}, UnknownGlobalKeys(path))
}

func TestLoadGlobal_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(":::invalid yaml"), 0644))

	_, err := LoadGlobal(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parsing global config")
}

func TestSave_CreatesDirectoryAndFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "config.yaml")

	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: "/home/user/repo1", Name: "repo1"},
		},
		ActiveProject: "test",
	}
	gc.SetConfigPath(path)

	require.NoError(t, gc.Save())

	// Verify file was created.
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var loaded GlobalConfig
	require.NoError(t, yaml.Unmarshal(data, &loaded))
	assert.Equal(t, "test", loaded.ActiveProject)
	assert.Len(t, loaded.Repos, 1)
	assert.Equal(t, "repo1", loaded.Repos[0].Name)
}

func TestSave_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	gc := &GlobalConfig{
		Projects: map[string]ProjectConfig{
			"proj-1": {
				Repos: []RepoEntry{
					{Path: "/a/b", Name: "b", Ref: "work"},
				},
			},
		},
		Repos: []RepoEntry{
			{Path: "/x/y", Name: "y"},
		},
		ActiveProject: "proj-1",
	}
	gc.SetConfigPath(path)
	require.NoError(t, gc.Save())

	loaded, err := LoadGlobal(path)
	require.NoError(t, err)
	assert.Equal(t, gc.ActiveProject, loaded.ActiveProject)
	assert.Equal(t, gc.Repos, loaded.Repos)
	assert.Equal(t, gc.Projects, loaded.Projects)
}

func TestValidate_ValidConfig(t *testing.T) {
	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: "/a", Name: "repo-a"},
			{Path: "/b", Name: "repo-b"},
		},
		Projects: map[string]ProjectConfig{
			"valid-project_1": {
				Repos: []RepoEntry{
					{Path: "/c", Name: "repo-c"},
				},
			},
		},
	}
	assert.NoError(t, gc.Validate())
}

func TestValidate_DuplicatePrefixes(t *testing.T) {
	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: "/a", Name: "same"},
			{Path: "/b", Name: "same"},
		},
	}
	err := gc.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate repo prefix")
	assert.Contains(t, err.Error(), "same")
}

func TestValidate_DuplicatePrefixesDerived(t *testing.T) {
	// Two repos with same last path component and no explicit name.
	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: "/home/user1/myrepo"},
			{Path: "/home/user2/myrepo"},
		},
	}
	err := gc.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate repo prefix")
	assert.Contains(t, err.Error(), "myrepo")
}

func TestValidate_InvalidProjectName(t *testing.T) {
	gc := &GlobalConfig{
		Projects: map[string]ProjectConfig{
			"valid-name":   {Repos: []RepoEntry{}},
			"invalid name": {Repos: []RepoEntry{}},
		},
	}
	err := gc.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid project name")
	assert.Contains(t, err.Error(), "invalid name")
}

func TestValidate_ConflictingNameOverrides(t *testing.T) {
	gc := &GlobalConfig{
		Projects: map[string]ProjectConfig{
			"proj-a": {
				Repos: []RepoEntry{
					{Path: "/shared/lib", Name: "lib-a"},
				},
			},
			"proj-b": {
				Repos: []RepoEntry{
					{Path: "/shared/lib", Name: "lib-b"},
				},
			},
		},
	}
	err := gc.Validate()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "conflicting name overrides")
}

func TestValidate_SharedRepoSameName(t *testing.T) {
	gc := &GlobalConfig{
		Projects: map[string]ProjectConfig{
			"proj-a": {
				Repos: []RepoEntry{
					{Path: "/shared/lib", Name: "lib"},
				},
			},
			"proj-b": {
				Repos: []RepoEntry{
					{Path: "/shared/lib", Name: "lib"},
				},
			},
		},
	}
	assert.NoError(t, gc.Validate())
}

func TestResolvePrefix(t *testing.T) {
	tests := []struct {
		entry  RepoEntry
		expect string
	}{
		{RepoEntry{Path: "/home/user/gortex", Name: "gortex"}, "gortex"},
		{RepoEntry{Path: "/home/user/gortex"}, "gortex"},
		{RepoEntry{Path: "/home/user/my-lib", Name: ""}, "my-lib"},
		{RepoEntry{Path: "/a/b/c"}, "c"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expect, ResolvePrefix(tt.entry))
	}
}

func TestAddRepo(t *testing.T) {
	gc := &GlobalConfig{}

	// Add a repo.
	err := gc.AddRepo(RepoEntry{Path: t.TempDir(), Name: "repo1"})
	require.NoError(t, err)
	assert.Len(t, gc.Repos, 1)

	// Add the same path again — should be skipped.
	err = gc.AddRepo(RepoEntry{Path: gc.Repos[0].Path, Name: "repo1-dup"})
	require.NoError(t, err)
	assert.Len(t, gc.Repos, 1) // still 1

	// Add a different repo.
	err = gc.AddRepo(RepoEntry{Path: t.TempDir(), Name: "repo2"})
	require.NoError(t, err)
	assert.Len(t, gc.Repos, 2)
}

func TestRemoveRepo(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: dir1, Name: "repo1"},
			{Path: dir2, Name: "repo2"},
		},
	}

	err := gc.RemoveRepo(dir1)
	require.NoError(t, err)
	assert.Len(t, gc.Repos, 1)
	assert.Equal(t, "repo2", gc.Repos[0].Name)

	// Remove non-existent.
	err = gc.RemoveRepo("/nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "repository not found")
}

func TestResolveRepos_TopLevel(t *testing.T) {
	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: "/a", Name: "a"},
			{Path: "/b", Name: "b"},
		},
	}

	repos, err := gc.ResolveRepos("")
	require.NoError(t, err)
	assert.Len(t, repos, 2)
}

func TestResolveRepos_Project(t *testing.T) {
	gc := &GlobalConfig{
		Projects: map[string]ProjectConfig{
			"my-proj": {
				Repos: []RepoEntry{
					{Path: "/c", Name: "c"},
				},
			},
		},
	}

	repos, err := gc.ResolveRepos("my-proj")
	require.NoError(t, err)
	assert.Len(t, repos, 1)
	assert.Equal(t, "c", repos[0].Name)
}

func TestResolveRepos_ProjectNotFound(t *testing.T) {
	gc := &GlobalConfig{
		Projects: map[string]ProjectConfig{
			"existing": {Repos: []RepoEntry{}},
		},
	}

	_, err := gc.ResolveRepos("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "project not found")
	assert.Contains(t, err.Error(), "existing")
}

// TestResolveRepos_PerRepoProjectTags verifies that ResolveRepos treats
// per-repo `project: <slug>` annotations as a valid project membership
// when the top-level Projects map has no matching entry. This is the
// shape produced by `gortex track --project <slug>`.
func TestResolveRepos_PerRepoProjectTags(t *testing.T) {
	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: "/a", Name: "a", Project: "alpha"},
			{Path: "/b", Name: "b", Project: "alpha"},
			{Path: "/c", Name: "c", Project: "alpha"},
			{Path: "/d", Name: "d", Project: "beta"},
			{Path: "/e", Name: "e", Project: "beta"},
		},
	}

	alpha, err := gc.ResolveRepos("alpha")
	require.NoError(t, err)
	assert.Len(t, alpha, 3)
	assert.Equal(t, []string{"a", "b", "c"}, []string{alpha[0].Name, alpha[1].Name, alpha[2].Name})

	beta, err := gc.ResolveRepos("beta")
	require.NoError(t, err)
	assert.Len(t, beta, 2)
	assert.Equal(t, []string{"d", "e"}, []string{beta[0].Name, beta[1].Name})

	_, err = gc.ResolveRepos("gamma")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project not found")
}

// TestResolveRepos_ProjectsMapWinsOverPerRepoTags verifies that when a
// project name is defined both via the top-level Projects map and via
// per-repo annotations on different entries, the explicit Projects map
// definition wins. Preserving this precedence keeps existing configs
// behaving exactly as before.
func TestResolveRepos_ProjectsMapWinsOverPerRepoTags(t *testing.T) {
	gc := &GlobalConfig{
		Projects: map[string]ProjectConfig{
			"alpha": {
				Repos: []RepoEntry{
					{Path: "/from-projects", Name: "from-projects"},
				},
			},
		},
		Repos: []RepoEntry{
			{Path: "/from-tags-1", Name: "from-tags-1", Project: "alpha"},
			{Path: "/from-tags-2", Name: "from-tags-2", Project: "alpha"},
		},
	}

	repos, err := gc.ResolveRepos("alpha")
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, "from-projects", repos[0].Name)
}

// TestResolveRepos_BothEmpty verifies the "project not found" error path
// when neither the Projects map nor any per-repo annotation matches.
func TestResolveRepos_BothEmpty(t *testing.T) {
	gc := &GlobalConfig{
		Repos: []RepoEntry{
			{Path: "/a", Name: "a"}, // no Project annotation
		},
	}

	_, err := gc.ResolveRepos("missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project not found")
}

func TestConfigPath_Default(t *testing.T) {
	gc := &GlobalConfig{}
	path := gc.ConfigPath()
	assert.Contains(t, path, "config.yaml")
	assert.Contains(t, path, "gortex")
}

func TestConfigPath_Override(t *testing.T) {
	gc := &GlobalConfig{}
	gc.SetConfigPath("/custom/path.yaml")
	assert.Equal(t, "/custom/path.yaml", gc.ConfigPath())
}

// Feature: multi-repo-support, Property 1: GlobalConfig serialization round-trip

// genRepoEntry generates a random RepoEntry with valid field values.
func genRepoEntry() *rapid.Generator[RepoEntry] {
	return rapid.Custom(func(t *rapid.T) RepoEntry {
		path := rapid.StringMatching(`/[a-z][a-z0-9/]{1,30}`).Draw(t, "path")
		hasName := rapid.Bool().Draw(t, "hasName")
		name := ""
		if hasName {
			name = rapid.StringMatching(`[a-zA-Z0-9_-]{1,20}`).Draw(t, "name")
		}
		hasRef := rapid.Bool().Draw(t, "hasRef")
		ref := ""
		if hasRef {
			ref = rapid.SampledFrom([]string{"work", "personal", "opensource", "dev", "staging"}).Draw(t, "ref")
		}
		return RepoEntry{
			Path: path,
			Name: name,
			Ref:  ref,
		}
	})
}

// genProjectConfig generates a random ProjectConfig with 0-5 repos.
func genProjectConfig() *rapid.Generator[ProjectConfig] {
	return rapid.Custom(func(t *rapid.T) ProjectConfig {
		n := rapid.IntRange(0, 5).Draw(t, "numRepos")
		repos := make([]RepoEntry, n)
		for i := range n {
			repos[i] = genRepoEntry().Draw(t, "repo")
		}
		return ProjectConfig{Repos: repos}
	})
}

// genGlobalConfig generates a random GlobalConfig with projects, repos, and active project.
func genGlobalConfig() *rapid.Generator[GlobalConfig] {
	return rapid.Custom(func(t *rapid.T) GlobalConfig {
		// Generate top-level repos (0-5).
		numRepos := rapid.IntRange(0, 5).Draw(t, "numRepos")
		repos := make([]RepoEntry, numRepos)
		for i := range numRepos {
			repos[i] = genRepoEntry().Draw(t, "repo")
		}

		// Generate projects (0-5) with valid names.
		numProjects := rapid.IntRange(0, 5).Draw(t, "numProjects")
		var projects map[string]ProjectConfig
		var projectNames []string
		if numProjects > 0 {
			projects = make(map[string]ProjectConfig, numProjects)
			for i := 0; i < numProjects; i++ {
				name := rapid.StringMatching(`[a-zA-Z][a-zA-Z0-9_-]{0,19}`).Draw(t, "projectName")
				// Avoid duplicate keys by appending index if needed.
				if _, exists := projects[name]; exists {
					name = name + rapid.StringMatching(`[0-9]{1,3}`).Draw(t, "suffix")
				}
				projects[name] = genProjectConfig().Draw(t, "project")
				projectNames = append(projectNames, name)
			}
		}

		// Active project: empty or one of the project names.
		activeProject := ""
		if len(projectNames) > 0 && rapid.Bool().Draw(t, "hasActiveProject") {
			activeProject = rapid.SampledFrom(projectNames).Draw(t, "activeProject")
		}

		return GlobalConfig{
			Projects:      projects,
			Repos:         repos,
			ActiveProject: activeProject,
		}
	})
}

// TestPropertyGlobalConfigSerializationRoundTrip verifies that serializing a GlobalConfig
// to YAML and deserializing it back produces an equivalent struct.
func TestPropertyGlobalConfigSerializationRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		original := genGlobalConfig().Draw(rt, "globalConfig")

		// Marshal to YAML.
		data, err := yaml.Marshal(&original)
		if err != nil {
			rt.Fatalf("failed to marshal GlobalConfig to YAML: %v", err)
		}

		// Unmarshal back.
		var restored GlobalConfig
		err = yaml.Unmarshal(data, &restored)
		if err != nil {
			rt.Fatalf("failed to unmarshal GlobalConfig from YAML: %v", err)
		}

		// Compare exported fields (configPath has yaml:"-" tag, so it won't round-trip).
		// Normalize nil vs empty for maps and slices.
		if original.Projects == nil {
			original.Projects = nil
		}
		if restored.Projects == nil {
			restored.Projects = nil
		}

		assert.Equal(rt, original.ActiveProject, restored.ActiveProject,
			"ActiveProject mismatch")
		assert.Equal(rt, len(original.Repos), len(restored.Repos),
			"Repos count mismatch")
		for i, want := range original.Repos {
			got := restored.Repos[i]
			assert.Equal(rt, want.Path, got.Path, "Repos[%d].Path mismatch", i)
			assert.Equal(rt, want.Name, got.Name, "Repos[%d].Name mismatch", i)
			assert.Equal(rt, want.Ref, got.Ref, "Repos[%d].Ref mismatch", i)
		}
		assert.Equal(rt, len(original.Projects), len(restored.Projects),
			"Projects count mismatch")
		for name, wantProj := range original.Projects {
			gotProj, ok := restored.Projects[name]
			assert.True(rt, ok, "Project %q missing after round-trip", name)
			if !ok {
				continue
			}
			assert.Equal(rt, len(wantProj.Repos), len(gotProj.Repos),
				"Project %q repos count mismatch", name)
			for i, want := range wantProj.Repos {
				got := gotProj.Repos[i]
				assert.Equal(rt, want.Path, got.Path,
					"Project %q Repos[%d].Path mismatch", name, i)
				assert.Equal(rt, want.Name, got.Name,
					"Project %q Repos[%d].Name mismatch", name, i)
				assert.Equal(rt, want.Ref, got.Ref,
					"Project %q Repos[%d].Ref mismatch", name, i)
			}
		}
	})
}

// Feature: multi-repo-support, Property 13: Project name validation

// genValidProjectName generates a project name matching ^[a-zA-Z0-9_-]+$.
func genValidProjectName() *rapid.Generator[string] {
	return rapid.StringMatching(`[a-zA-Z0-9_-]{1,30}`)
}

// genInvalidProjectName generates a project name containing at least one character outside [a-zA-Z0-9_-].
func genInvalidProjectName() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		// Build a name with a mix of valid chars and at least one invalid char.
		validPart := rapid.StringMatching(`[a-zA-Z0-9_-]{0,10}`).Draw(t, "validPrefix")
		// Pick an invalid character: space, dot, slash, @, !, etc.
		invalidChar := rapid.SampledFrom([]string{" ", ".", "/", "@", "!", "#", "$", "%", "^", "&", "*", "(", ")", "+", "=", "[", "]", "{", "}", "|", "\\", ":", ";", "'", "\"", "<", ">", ",", "?", "~", "`"}).Draw(t, "invalidChar")
		suffix := rapid.StringMatching(`[a-zA-Z0-9_-]{0,10}`).Draw(t, "validSuffix")
		return validPart + invalidChar + suffix
	})
}

// TestPropertyProjectNameValidation verifies that Validate() rejects project names
// with characters outside [a-zA-Z0-9_-] and accepts valid names.
func TestPropertyProjectNameValidation(t *testing.T) {
	validNameRe := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

	rapid.Check(t, func(rt *rapid.T) {
		// Randomly choose whether to test a valid or invalid name.
		useValid := rapid.Bool().Draw(rt, "useValid")

		var projectName string
		if useValid {
			projectName = genValidProjectName().Draw(rt, "validName")
		} else {
			projectName = genInvalidProjectName().Draw(rt, "invalidName")
		}

		gc := &GlobalConfig{
			Projects: map[string]ProjectConfig{
				projectName: {Repos: []RepoEntry{}},
			},
		}

		err := gc.Validate()

		if validNameRe.MatchString(projectName) {
			// Valid name — Validate should pass (at least no project name error).
			if err != nil {
				assert.NotContains(rt, err.Error(), "invalid project name",
					"valid project name %q should not trigger 'invalid project name' error", projectName)
			}
		} else {
			// Invalid name — Validate must return an error mentioning "invalid project name".
			assert.Error(rt, err, "expected error for invalid project name %q", projectName)
			assert.Contains(rt, err.Error(), "invalid project name",
				"error should mention 'invalid project name' for %q", projectName)
		}
	})
}

// Feature: multi-repo-support, Property 3: Duplicate prefix validation

// TestPropertyDuplicatePrefixValidation verifies that Validate() returns an error
// if and only if two or more RepoEntry values resolve to the same prefix.
func TestPropertyDuplicatePrefixValidation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Step 1: Generate a base list of unique prefixes.
		numEntries := rapid.IntRange(2, 8).Draw(rt, "numEntries")
		prefixes := make([]string, numEntries)
		seen := make(map[string]bool)
		for i := range numEntries {
			for {
				p := rapid.StringMatching(`[a-zA-Z][a-zA-Z0-9_-]{0,14}`).Draw(rt, "prefix")
				if !seen[p] {
					seen[p] = true
					prefixes[i] = p
					break
				}
			}
		}

		// Step 2: Build RepoEntry values with unique paths and explicit names set to the prefix.
		entries := make([]RepoEntry, numEntries)
		for i, p := range prefixes {
			// Use a unique path per entry so paths never collide.
			entries[i] = RepoEntry{
				Path: fmt.Sprintf("/repos/%d/%s", i, p),
				Name: p,
			}
		}

		// Step 3: Optionally inject a duplicate prefix.
		injectDuplicate := rapid.Bool().Draw(rt, "injectDuplicate")
		if injectDuplicate {
			// Pick a random existing prefix and assign it to a different entry.
			srcIdx := rapid.IntRange(0, numEntries-1).Draw(rt, "srcIdx")
			dstIdx := rapid.IntRange(0, numEntries-1).Draw(rt, "dstIdx")
			// Ensure src and dst are different entries.
			if dstIdx == srcIdx {
				dstIdx = (dstIdx + 1) % numEntries
			}
			entries[dstIdx].Name = entries[srcIdx].Name
		}

		// Step 4: Construct GlobalConfig and call Validate().
		gc := &GlobalConfig{
			Repos: entries,
		}
		err := gc.Validate()

		// Step 5: Determine expected outcome by checking for actual duplicates.
		resolvedPrefixes := make(map[string]bool)
		hasDuplicate := false
		for _, e := range entries {
			rp := ResolvePrefix(e)
			if resolvedPrefixes[rp] {
				hasDuplicate = true
				break
			}
			resolvedPrefixes[rp] = true
		}

		if hasDuplicate {
			assert.Error(rt, err, "expected error for duplicate prefixes")
			assert.Contains(rt, err.Error(), "duplicate repo prefix",
				"error should mention 'duplicate repo prefix'")
		} else {
			assert.NoError(rt, err, "expected no error when all prefixes are unique")
		}
	})
}
