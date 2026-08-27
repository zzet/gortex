package agents

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// render.go is the engine behind the skill-render drift fence. It runs
// every adapter against an isolated sandbox (a throwaway HOME + repo
// root) with ForceDetect on — so the adapter renders regardless of which
// tools are installed — then serialises the produced file tree into a
// stable, machine-independent text manifest. The manifest is
// byte-compared against committed goldens by the drift test and the
// `gortex agents render` command, so any change to an adapter's
// generated MCP config, instructions, hooks, or skills surfaces as a
// reviewable diff across every platform, not just Claude.
//
// What the fence covers (deliberately, and why):
//
//   - Both install modes. Every adapter is rendered twice — once in
//     ModeProject (`gortex init`) and once in ModeGlobal (`gortex
//     install`) — into two separate sandboxes, and the two manifests are
//     concatenated. Both passes walk the sandbox HOME as well as the repo
//     root, so a project-mode write into the user's home (codex's
//     ~/.codex/config.toml) was always fenced; what was not fenced is
//     everything behind the ModeGlobal branch — ~/.claude/skills, the
//     user-level rule block and instruction profiles, the user-scope MCP
//     registration.
//   - Lifecycle hooks. InstallHooks is true, so each adapter's hook
//     stanza is pinned. Note that Env.HookMode is honoured *only* by the
//     claudecode adapter; every other adapter ignores it, so it is left
//     empty here and no golden should be read as pinning a non-Claude
//     hook posture.
//   - Generated community skills. A fixed two-entry GeneratedSkills
//     fixture makes per-community skill materialisation render, and its
//     DirName values are valid slugs (^[a-z0-9]+(-[a-z0-9]+)*$) so
//     adapters that reject malformed skill directory names (OpenCode,
//     Copilot) are exercised on a name they must accept.
//
// Hermeticity and determinism are load-bearing here: a render must
// never touch the developer's real HOME, and must never encode a value
// that came from the regenerating developer's environment. renderPass
// therefore pins the process environment (see pinRenderEnv) around
// every Apply. A render that escapes the sandbox both corrupts the
// developer's machine and renders an empty home/ manifest section, so
// the escape hides itself — cmd/gortex's render tests assert against
// exactly that.
//
// Consequence worth knowing before regenerating: because the ModeGlobal
// pass materialises the instruction profiles into the sandbox HOME, the
// claude-code golden now contains the profile bodies. Editing
// internal/profiles prose therefore shows up as a claude-code golden
// diff — that is the fence working, not noise.

// RenderManifest renders each adapter into its own sandbox and returns a
// normalised manifest per adapter, keyed by adapter name.
func RenderManifest(adapters []Adapter) (map[string]string, error) {
	out := make(map[string]string, len(adapters))
	for _, a := range adapters {
		m, err := renderOne(a)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", a.Name(), err)
		}
		out[a.Name()] = m
	}
	return out, nil
}

// renderPasses are the install modes every adapter is rendered through,
// in emission order. Each pass gets its own sandbox and contributes a
// block of mode-prefixed manifest keys.
//
// Key scheme: every manifest key is `<mode>/<home|root>/<relative
// path>` — e.g. `project/root/.mcp.json`, `global/home/.claude/
// settings.json`. Prefixing each key (rather than fencing the two
// passes behind section banners) was chosen because it keeps every
// manifest line self-describing: a grep for one line of a golden says
// which mode produced it, and because the two blocks are serialised
// independently a change confined to one mode produces a diff hunk
// confined to that block instead of shifting the other mode's lines.
// Project is emitted first because that is the order a user meets them
// (`gortex init`, then `gortex install`).
var renderPasses = []struct {
	prefix string
	mode   Mode
}{
	{prefix: "project", mode: ModeProject},
	{prefix: "global", mode: ModeGlobal},
}

// renderSkillsRouting is the fixed community-routing payload every pass
// feeds adapters, so the routing blocks render deterministically.
const renderSkillsRouting = "- [example-community](.claude/skills/example/SKILL.md) — example routing block\n"

// renderGeneratedSkills is the fixed per-community skill fixture. Two
// entries (not one) so ordering / separator bugs in adapters that
// serialise a list of skills surface in the golden. The DirName values
// are deliberately valid skill slugs — `^[a-z0-9]+(-[a-z0-9]+)*$` —
// because OpenCode and Copilot refuse to write anything else, and this
// fixture is what proves they accept what the generator produces.
func renderGeneratedSkills() []GeneratedSkill {
	body := func(label string) string {
		return "---\nname: gortex-" + label + "\n" +
			"description: Example generated community skill used by the render drift fence.\n---\n\n" +
			"# " + label + "\n\nRouting stub for the " + label + " community.\n"
	}
	return []GeneratedSkill{
		{CommunityID: "c1", Label: "example-one", DirName: "gortex-example-one", Content: body("example-one")},
		{CommunityID: "c2", Label: "example-two", DirName: "gortex-example-two", Content: body("example-two")},
	}
}

// renderOne applies a single adapter once per install mode and returns
// the concatenated manifest. Each pass renders into its own sandbox so
// the project pass's files never leak into the global pass's section.
func renderOne(a Adapter) (string, error) {
	var b strings.Builder
	for _, pass := range renderPasses {
		m, err := renderPass(a, pass.mode, pass.prefix)
		if err != nil {
			return "", fmt.Errorf("%s mode: %w", pass.prefix, err)
		}
		b.WriteString(m)
	}
	return b.String(), nil
}

// renderPass applies a single adapter for one mode in a fresh sandbox
// and returns its mode-prefixed manifest. The HOME and repo-root temp
// dirs are removed afterwards, and the process environment is restored
// before returning.
func renderPass(a Adapter, mode Mode, prefix string) (string, error) {
	home, err := os.MkdirTemp("", "gortex-render-home-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(home) }()
	root, err := os.MkdirTemp("", "gortex-render-root-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(root) }()

	restoreEnv := pinRenderEnv(home)
	defer restoreEnv()

	env := Env{
		Root:         root,
		Home:         home,
		Mode:         mode,
		HookCommand:  "gortex hook",
		InstallHooks: true,
		// Only meaningful in ModeGlobal (adapters ignore it elsewhere);
		// setting it there is what makes the global rule block and the
		// generated instruction profiles part of the fence.
		InstallGlobalInstructions: mode == ModeGlobal,
		// Hermetic instruction-profile dir. Without this
		// InstructionsDir(env) falls back to profiles.DefaultDir() —
		// the real ~/.gortex/instructions — and the ModeGlobal render
		// would both write to the developer's machine and encode
		// whichever profile they last switched to.
		InstructionsDir: filepath.Join(home, ".gortex", "instructions"),
		SkillsRouting:   renderSkillsRouting,
		GeneratedSkills: renderGeneratedSkills(),
		Stderr:          io.Discard,
	}
	if _, err := a.Apply(env, ApplyOpts{ForceDetect: true}); err != nil {
		return "", err
	}
	return manifestForDirs(home, root, prefix+"/")
}

// renderEnvPins is the process environment every render pass runs
// under. It closes two classes of bug that only appear once ModeGlobal
// and hooks are part of the fence:
//
//   - Sandbox escapes. $CLAUDE_CONFIG_DIR relocates the whole Claude
//     Code user-level write (see claudecode.userClaudeConfigDir), and
//     $KIMI_CODE_HOME does the same for Kimi. On a machine exporting
//     either, the render would write into the developer's real config
//     dir and the manifest's home/ section would render empty — an
//     escape that hides itself. $HOME / $USERPROFILE / the Windows
//     AppData pair are pinned and the XDG bases cleared too, so even an
//     adapter that ignores Env.Home and calls os.UserHomeDir() /
//     platform.DataDir() lands in the sandbox.
//   - Env-sourced content. codex's hook command embeds
//     $GORTEX_CODEX_HOOK_MODE at render time and profiles.ActiveName
//     honours $GORTEX_INSTRUCTIONS_PROFILE, so without pinning, the
//     goldens would encode the posture/profile of whoever regenerated
//     them and CI would fail on a diff nobody can explain.
//
// The higher-precedence claudecode.SetConfigDirOverride seam cannot be
// driven from here — claudecode imports this package, so the import can
// only go the other way. The render tests in cmd/gortex clear that seam
// (it is only ever set by `gortex install --claude-config-dir`, never
// on the render path) so the pins below are authoritative.
//
// The config-dir variables are *unset* rather than repointed at the
// sandbox, because they do more than relocate a directory:
// $CLAUDE_CONFIG_DIR also moves ~/.claude.json into that directory, so
// pinning it would make the goldens encode a layout no default install
// produces. Unset plus a pinned $HOME lands every adapter on its real
// default path, inside the sandbox.
//
// An empty value means "unset for the duration".
func renderEnvPins(home string) map[string]string {
	return map[string]string{
		"HOME":        home, // Unix: os.UserHomeDir
		"USERPROFILE": home, // Windows: os.UserHomeDir
		// Windows: os.UserConfigDir / os.UserCacheDir read these two
		// and ignore the XDG variables entirely.
		"AppData":      filepath.Join(home, "AppData", "Roaming"),
		"LocalAppData": filepath.Join(home, "AppData", "Local"),

		"CLAUDE_CONFIG_DIR":           "",
		"KIMI_CODE_HOME":              "",
		"GORTEX_CODEX_HOOK_MODE":      "",
		"GORTEX_INSTRUCTIONS_PROFILE": "",
		"XDG_CONFIG_HOME":             "",
		"XDG_DATA_HOME":               "",
		"XDG_CACHE_HOME":              "",
		"XDG_STATE_HOME":              "",
	}
}

// pinRenderEnv applies renderEnvPins and returns a restore func that
// puts every touched variable back exactly as it was (including
// "was not set at all"). Renders are serial, so a plain
// save/set/restore is enough.
func pinRenderEnv(home string) func() {
	pins := renderEnvPins(home)
	prev := make(map[string]*string, len(pins))
	for k, v := range pins {
		if old, ok := os.LookupEnv(k); ok {
			saved := old
			prev[k] = &saved
		} else {
			prev[k] = nil
		}
		if v == "" {
			_ = os.Unsetenv(k)
			continue
		}
		_ = os.Setenv(k, v)
	}
	return func() {
		for k, old := range prev {
			if old == nil {
				_ = os.Unsetenv(k)
				continue
			}
			_ = os.Setenv(k, *old)
		}
	}
}

// manifestForDirs walks the sandbox HOME and repo root and builds a
// sorted, normalised manifest. Each file becomes a `=== <key> ===`
// header followed by its (sandbox-path-stripped) content; entries are
// sorted by key so the manifest is deterministic. keyPrefix is the
// mode label ("project/" or "global/") every key carries.
func manifestForDirs(home, root, keyPrefix string) (string, error) {
	type entry struct{ key, content string }
	var entries []entry

	collect := func(base, prefix string) error {
		prefix = keyPrefix + prefix
		return filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(base, path)
			if err != nil {
				return err
			}
			entries = append(entries, entry{
				key:     canonicalManifestKey(prefix + filepath.ToSlash(rel)),
				content: normalizeRender(string(data), home, root),
			})
			return nil
		})
	}
	if err := collect(home, "home/"); err != nil {
		return "", err
	}
	if err := collect(root, "root/"); err != nil {
		return "", err
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "=== %s ===\n%s\n", e.key, strings.TrimRight(e.content, "\n"))
	}
	return b.String(), nil
}

// gortexBinaryPaths memoizes the absolute paths an adapter might bake
// into its config for the gortex binary — the running process
// (os.Executable, as the hermes adapter uses) and a PATH lookup. They
// are normalised to bare "gortex" so the manifest doesn't depend on
// where this build happens to live (dev box vs CI vs `go test` binary).
var gortexBinaryPaths = sync.OnceValue(func() []string {
	seen := map[string]struct{}{}
	var paths []string
	add := func(p string) {
		if p == "" || p == "gortex" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	if exe, err := os.Executable(); err == nil {
		add(exe)
		if abs, e := filepath.Abs(exe); e == nil {
			add(abs)
		}
	}
	if p, err := exec.LookPath("gortex"); err == nil {
		add(p)
	}
	return paths
})

// canonicalManifestKey rewrites the OS-specific user-config locations
// in a manifest path to a single canonical (Linux/XDG) form, so the
// manifest is identical regardless of the OS the render runs on.
// Without this an adapter that uses OS-specific config dirs makes the
// golden non-portable (e.g. zed writes ~/Library/Application Support/Zed
// on macOS, ~/AppData/Roaming/Zed on Windows, and ~/.config/zed on
// Linux). Only manifest keys are folded; file contents are compared
// verbatim.
func canonicalManifestKey(key string) string {
	key = strings.ReplaceAll(key, "Library/Application Support/", ".config/")
	key = strings.ReplaceAll(key, "AppData/Roaming/", ".config/")
	// zed capitalises its directory on macOS/Windows but not on Linux.
	key = strings.ReplaceAll(key, ".config/Zed/", ".config/zed/")
	return key
}

// normalizeRender replaces machine-specific absolutes — the sandbox
// HOME / repo root and the resolved gortex binary path — with stable
// placeholders so the manifest is identical on every machine.
func normalizeRender(s, home, root string) string {
	// The values to scrub are native paths — os.Executable, exec.LookPath,
	// the sandbox HOME — but a rendered manifest deliberately carries
	// '/'-spelled paths even on Windows: shellSafeHookBinary normalises
	// unconditionally, and an @-include is document content rather than a
	// filesystem call. Replacing only the native spelling therefore leaves
	// a machine-specific absolute in the manifest on Windows and the
	// golden drifts for every developer who has gortex installed.
	// filepath.ToSlash is a no-op on POSIX, so the second pass collapses
	// into the first everywhere else.
	scrub := func(s, from, to string) string {
		if from == "" {
			return s
		}
		s = strings.ReplaceAll(s, from, to)
		return strings.ReplaceAll(s, filepath.ToSlash(from), to)
	}
	s = scrub(s, home, "$HOME")
	s = scrub(s, root, "$ROOT")
	for _, p := range gortexBinaryPaths() {
		s = scrub(s, p, "gortex")
	}
	return s
}

// RenderContainsRegistration reports whether a rendered manifest still
// wires Gortex in — an MCP server stanza, a gortex hook, a community
// routing block, or instruction prose all reference "gortex". The drift
// CLI uses it as a structural sanity check (independent of the byte
// golden) that an adapter didn't silently stop emitting gortex content.
func RenderContainsRegistration(manifest string) bool {
	return strings.Contains(strings.ToLower(manifest), "gortex")
}
