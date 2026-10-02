// Package pi implements the Gortex init integration for the Pi coding
// agent (earendil-works/pi, a.k.a. pi-mono).
//
// Pi has no MCP support, by design, so the bridge between the two is a Pi
// package: `pi-gortex` on npm. It registers Gortex's graph tools as
// native Pi tools over a persistent `gortex mcp` child and forwards Pi's
// tool calls to `gortex hook --agent=pi` for read discipline. This
// adapter ships none of that code. It declares the package in Pi's
// settings and writes the sidecar the package resolves its posture from
// at runtime.
//
// File layout written by the adapter:
//
//	ModeProject: <root>/.pi/settings.json            packages entry
//	             <root>/.pi/gortex.json              sidecar, non-defaults only
//	             <root>/AGENTS.md                    communities block, --skills only
//	ModeGlobal:  <agentDir>/settings.json            packages entry
//	             <agentDir>/extensions/gortex.json   sidecar, non-defaults only
//
// Both modes also take back the extension earlier releases rendered
// under those trees. That whole path lives in legacy.go.
//
// Read-discipline rules are injected by the package at runtime, not
// written to an instructions file. AGENTS.md is touched only for the
// community-routing block, and only with --skills.
package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const Name = "pi"
const DocsURL = "https://github.com/gortexhq/pi#readme"

// PackageSpec is the entry appended to Pi's `packages`.
const PackageSpec = "npm:pi-gortex"

// packageName is PackageSpec's npm identity, used to recognise an entry a
// user already added in any of the spellings Pi accepts.
const packageName = "pi-gortex"

// The package's own fallbacks. A sidecar key is written only when it
// differs from one of these, because a file restating them is noise in a
// path teams commit.
const (
	defaultBin      = "gortex"
	defaultHookMode = "deny"
)

const (
	defaultAgentDir  = ".pi/agent"
	sidecarBaseName  = "gortex.json"
	settingsBaseName = "settings.json"
)

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect reports whether Pi is in use: a project-local `.pi/` dir, a
// user-level `~/.pi/`, or the `pi` CLI on PATH.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if env.Mode == agents.ModeProject && env.Root != "" {
		if _, err := os.Stat(filepath.Join(env.Root, ".pi")); err == nil {
			return true, nil
		}
	}
	if env.Home != "" {
		if _, err := os.Stat(filepath.Join(env.Home, ".pi")); err == nil {
			return true, nil
		}
	}
	for _, bin := range []string{"pi", "pi-coding-agent"} {
		if p, err := exec.LookPath(bin); err == nil && p != "" {
			return true, nil
		}
	}
	return false, nil
}

// AgentDir mirrors config.ts::defaultAgentDir in the package: the
// $PI_CODING_AGENT_DIR override wins, with the same `~` expansion Pi
// applies, and <home>/.pi/agent is the fallback. Exported because the
// uninstall path resolves the same directory.
func AgentDir(home string) string {
	if override := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); override != "" {
		switch {
		case override == "~":
			return home
		case strings.HasPrefix(override, "~/"):
			return filepath.Join(home, filepath.FromSlash(override[2:]))
		default:
			return override
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, filepath.FromSlash(defaultAgentDir))
}

// settingsPath is the Pi settings file the packages entry lands in.
func settingsPath(env agents.Env) string {
	if env.Mode == agents.ModeGlobal {
		return filepath.Join(AgentDir(env.Home), settingsBaseName)
	}
	return filepath.Join(env.Root, ".pi", settingsBaseName)
}

// sidecarPath is where the package looks for its configuration. The two
// scopes are not symmetric: project settings live in `.pi/` while the
// global sidecar sits inside the agent dir's extensions folder.
func sidecarPath(env agents.Env) string {
	if env.Mode == agents.ModeGlobal {
		return GlobalSidecarPath(env.Home)
	}
	return filepath.Join(env.Root, ".pi", sidecarBaseName)
}

// GlobalSidecarPath is the user-level sidecar, shared with uninstall.
func GlobalSidecarPath(home string) string {
	dir := AgentDir(home)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "extensions", sidecarBaseName)
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	p := &agents.Plan{}

	settings := settingsPath(env)
	switch {
	case hasPackageEntry(settings):
		p.Files = append(p.Files, agents.FileAction{
			Path: settings, Action: agents.ActionSkip, Reason: "already-configured",
		})
	case !settingsAreMergeable(settings):
		p.Files = append(p.Files, agents.FileAction{
			Path: settings, Action: agents.ActionSkip, Reason: "unparseable",
		})
	default:
		p.Files = append(p.Files, agents.FileAction{
			Path: settings, Action: planWrite(settings), Keys: []string{"packages"},
		})
	}

	sidecar := sidecarPath(env)
	switch {
	case len(sidecarConfig(env)) > 0:
		p.Files = append(p.Files, agents.FileAction{Path: sidecar, Action: planWrite(sidecar)})
	case fileExists(sidecar):
		p.Files = append(p.Files, agents.FileAction{Path: sidecar, Action: agents.ActionWouldDelete})
	}

	// AGENTS.md gets the community-routing block, and only with skills
	// (project mode). Read-discipline rules ride the package, not this file.
	if env.Mode != agents.ModeGlobal && env.Root != "" && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path:   filepath.Join(env.Root, "AGENTS.md"),
			Action: agents.ActionWouldMerge, Keys: []string{"communities-block"},
		})
	}

	for _, path := range legacyExtensionPaths(env) {
		if ownsLegacyExtension(path) {
			p.Files = append(p.Files, agents.FileAction{Path: path, Action: agents.ActionWouldDelete})
		}
	}
	return p, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip Pi setup (pi not detected)")
		return res, nil
	}
	if env.Mode == agents.ModeGlobal && env.Home == "" {
		return res, fmt.Errorf("pi: global mode requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Pi integration...")

	// 1. The packages entry — what actually loads the bridge.
	settingsAction, err := mergePackages(env, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, settingsAction)

	// 2. The sidecar (Gortex owns it end-to-end, so a re-run resets the
	// posture to whatever this invocation's flags say).
	sidecarAction, wrote, err := writeSidecar(env, opts)
	if err != nil {
		return res, err
	}
	if wrote {
		res.Files = append(res.Files, sidecarAction)
	}

	// 3. Community routing → AGENTS.md (project mode, skills enabled only).
	if env.Mode != agents.ModeGlobal && env.Root != "" && env.SkillsRouting != "" {
		agentsMd := filepath.Join(env.Root, "AGENTS.md")
		routingAction, err := agents.UpsertMarkedBlock(env.Stderr, agentsMd, env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, routingAction)
	}

	// 4. Take back the extension earlier releases rendered.
	res.Files = append(res.Files, removeLegacyExtensions(env, opts)...)

	res.Configured = true
	return res, nil
}

// mergePackages appends PackageSpec to Pi's `packages` array. An entry
// naming pi-gortex in any spelling Pi accepts is left exactly as it is: a
// pinned version, a filter object or a local checkout are all deliberate
// choices, and normalising them would undo the user's intent.
func mergePackages(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := settingsPath(env)
	if !settingsAreMergeable(path) {
		internalutil.Warnf(env.Stderr,
			"keeping %s: this build cannot parse it. Add %q to its `packages` by hand",
			path, PackageSpec)
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "unparseable"}, nil
	}
	return agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		raw, present := root["packages"]
		list, ok := raw.([]any)
		if present && !ok {
			// Not an array: Pi cannot read it either, and reshaping
			// somebody's file is not this adapter's call.
			return false, nil
		}
		for _, entry := range list {
			if packageEntryName(entry, filepath.Dir(path)) == packageName {
				return false, nil
			}
		}
		root["packages"] = append(list, PackageSpec)
		return true, nil
	}, opts)
}

// settingsAreMergeable reports whether the settings file at path can take
// the entry without losing anything else in it. MergeJSON's fallback for
// a file encoding/json rejects is to back it up and write ours over the
// original; this file is the user's, and Pi may accept syntax this parser
// does not, so one we cannot read is left alone instead.
func settingsAreMergeable(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return true
	}
	var root map[string]any
	return json.Unmarshal(data, &root) == nil
}

// hasPackageEntry reports whether the settings file at path already
// declares pi-gortex. An unreadable or malformed file reads as "no",
// which is the direction that makes Plan agree with Apply.
func hasPackageEntry(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return false
	}
	list, _ := root["packages"].([]any)
	for _, entry := range list {
		if packageEntryName(entry, filepath.Dir(path)) == packageName {
			return true
		}
	}
	return false
}

// packageEntryName extracts the npm package name from one `packages`
// entry. Pi accepts a bare source string or an object carrying one under
// `source`; an npm source may or may not carry the `npm:` prefix and a
// trailing @version; and a local path is a checkout whose name lives in
// its package.json. baseDir is the settings file's directory, which is
// what Pi resolves a relative path against. Git and URL sources have no
// npm name and yield the empty string.
func packageEntryName(entry any, baseDir string) string {
	var source string
	switch v := entry.(type) {
	case string:
		source = v
	case map[string]any:
		source, _ = v["source"].(string)
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}

	if name, prefixed := strings.CutPrefix(source, "npm:"); prefixed {
		return trimPackageVersion(name)
	}
	if isLocalPathSource(source) {
		return localPackageName(source, baseDir)
	}
	// A scope is the only reason a bare npm name carries a slash.
	if !strings.HasPrefix(source, "@") && (strings.Contains(source, "/") || strings.Contains(source, ":")) {
		return ""
	}
	return trimPackageVersion(source)
}

// trimPackageVersion strips a trailing @version while leaving a leading
// @scope intact.
func trimPackageVersion(name string) string {
	if i := strings.LastIndex(name, "@"); i > 0 {
		return name[:i]
	}
	return name
}

func isLocalPathSource(source string) bool {
	return strings.HasPrefix(source, "./") || strings.HasPrefix(source, "../") ||
		strings.HasPrefix(source, "/") || filepath.IsAbs(source)
}

// localPackageName reads the name out of a checkout's package.json. A
// developer running against a local pi-gortex is the case this exists
// for: appending the npm spec beside their checkout would load the
// bridge twice, which is the failure the package migration removes.
func localPackageName(source, baseDir string) string {
	path := filepath.FromSlash(source)
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	data, err := os.ReadFile(filepath.Join(path, "package.json"))
	if err != nil {
		return ""
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ""
	}
	return manifest.Name
}

// sidecarConfig is the JSON the package reads, carrying only the values
// that differ from its own defaults. An install that changed nothing
// produces no file at all.
func sidecarConfig(env agents.Env) map[string]any {
	cfg := make(map[string]any)
	if bin := sidecarBin(env); bin != "" {
		cfg["bin"] = bin
	}
	if mode := normalizeMode(env.HookMode); mode != defaultHookMode {
		cfg["hook_mode"] = mode
	}
	if !env.InstallHooks {
		cfg["enforce"] = false
	}
	return cfg
}

// writeSidecar renders sidecarConfig, or deletes the file when there is
// nothing left to say. Gortex owns this path, so a plain re-run after an
// install that set a posture resets it rather than leaving a stale value
// the user has no reason to expect. Reports whether it acted at all, so
// the no-config case adds no action to an otherwise-idempotent run.
func writeSidecar(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, bool, error) {
	path := sidecarPath(env)
	cfg := sidecarConfig(env)

	if len(cfg) == 0 {
		if !fileExists(path) {
			return agents.FileAction{}, false, nil
		}
		action, err := deleteFile(env.Stderr, path, "[gortex init] removed %s", opts)
		return action, err == nil, err
	}

	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return agents.FileAction{}, false, fmt.Errorf("pi: marshal sidecar: %w", err)
	}
	action, err := agents.WriteOwnedFile(env.Stderr, path, string(body)+"\n", opts)
	return action, err == nil, err
}

// sidecarBin returns the gortex path to pin, or "" when a bare `gortex`
// resolves to the same file. The pin exists because an agent's PATH often
// lacks the directory gortex was installed into; writing it when PATH
// already finds it would put a machine-specific absolute path into a file
// teams commit.
func sidecarBin(env agents.Env) string {
	bin := resolveGortexBin(env)
	if bin == "" || bin == defaultBin {
		return ""
	}
	if onPath, err := exec.LookPath(defaultBin); err == nil && sameFile(onPath, bin) {
		return ""
	}
	return bin
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// deleteFile removes path, honouring DryRun, and reports the action.
func deleteFile(w io.Writer, path, format string, opts agents.ApplyOpts) (agents.FileAction, error) {
	if opts.DryRun {
		return agents.FileAction{Path: path, Action: agents.ActionWouldDelete}, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return agents.FileAction{}, fmt.Errorf("remove %s: %w", path, err)
	}
	internalutil.Logf(w, format, path)
	return agents.FileAction{Path: path, Action: agents.ActionDelete}, nil
}

// planWrite predicts whether a write lands as a create or a merge.
func planWrite(path string) agents.ActionKind {
	if fileExists(path) {
		return agents.ActionWouldMerge
	}
	return agents.ActionWouldCreate
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// resolveGortexBin prefers an explicit HookCommand binary, then a `gortex`
// on PATH, then the bare name. Only the binary matters here: the package
// builds the hook argv itself from the sidecar's posture.
func resolveGortexBin(env agents.Env) string {
	if env.HookCommand != "" {
		if fields := strings.Fields(env.HookCommand); len(fields) > 0 {
			return fields[0]
		}
	}
	if p, err := exec.LookPath(defaultBin); err == nil && p != "" {
		return p
	}
	return defaultBin
}

// normalizeMode mirrors the hook posture strings the daemon accepts; any
// unknown value (including empty) collapses to the deny default.
func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "enrich":
		return "enrich"
	case "consult-unlock":
		return "consult-unlock"
	case "nudge", "adaptive-nudge":
		return "nudge"
	default:
		return defaultHookMode
	}
}
