package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

// agentLabels maps adapter Name() → human-friendly label shown in the wizard
// checklist. Anything not listed falls through to title-casing the slug, so a
// new adapter that ships before this table is updated still renders sanely.
var agentLabels = map[string]string{
	"claude-code": "Claude Code",
	"cursor":      "Cursor",
	"aider":       "Aider",
	"antigravity": "Antigravity",
	"cline":       "Cline",
	"codex":       "Codex CLI",
	"continue":    "Continue.dev",
	"gemini":      "Gemini CLI",
	"hermes":      "Hermes",
	"kilocode":    "Kilo Code",
	"kiro":        "Kiro",
	"oh-my-pi":    "Oh My Pi",
	"opencode":    "OpenCode",
	"openclaw":    "OpenClaw",
	"vscode":      "VS Code (Copilot)",
	"windsurf":    "Windsurf",
	"zed":         "Zed",
}

// agentDetails is the one-line "what does Gortex write here" hint that
// appears in dim text right of each agent's label.
var agentDetails = map[string]string{
	"claude-code": ".mcp.json · .claude/ hooks · CLAUDE.md · skills",
	"cursor":      ".cursor/rules + .cursor/mcp.json",
	"aider":       ".aider.conf.yml",
	"antigravity": ".antigravity/mcp.json",
	"cline":       ".clinerules + cline_mcp_settings.json",
	"codex":       "AGENTS.md + config.toml",
	"continue":    ".continue/config.json",
	"gemini":      ".gemini/settings.json",
	"hermes":      "~/.hermes/config.yaml + profiles + skills",
	"kilocode":    ".kilocode/mcp.json",
	"kiro":        ".kiro/settings/mcp.json",
	"oh-my-pi":    ".omp/mcp.json",
	"opencode":    "opencode.json",
	"openclaw":    ".openclaw/mcp.json",
	"vscode":      ".vscode/mcp.json",
	"windsurf":    ".windsurf/mcp.json",
	"zed":         ".zed/settings.json",
}

// agentLabel returns a friendly display name for the given adapter slug.
func agentLabel(name string) string {
	if l, ok := agentLabels[name]; ok {
		return l
	}
	parts := strings.Split(name, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// agentDetail returns the dim "writes …" hint for the given adapter slug.
func agentDetail(name string) string {
	if d, ok := agentDetails[name]; ok {
		return d
	}
	return ""
}

// wizardStep is the current screen the user is looking at.
type wizardStep int

const (
	stepAgents wizardStep = iota
	stepOptions
	stepConfirm
	stepCancelled
)

// initWizardModel is the bubbletea Model for `gortex init -i` (and reused by
// `gortex install -i` with a different title). It drives the pre-flight
// selection screens; once the user confirms, runInit / runInstall takes over
// and the dashboard is rendered by a separate Dashboard model. Keeping the
// two models separate avoids the wizard having to know about indexer state.
type initWizardModel struct {
	step      wizardStep
	checklist tui.Checklist
	options   tui.OptionsPanel
	title     string // banner title, e.g. "gortex init" or "gortex install"
	rootPath  string // banner subtitle target ("this repo" / "your machine")

	tick      int
	width     int
	confirmed bool
	cancelled bool

	// Resolved choices — read by the caller after Run() returns.
	pickedAgents  []string
	hooks         bool
	hookMode      string
	analyze       bool
	skills        bool
	telemetry     bool
	showTelemetry bool
}

// initialDetection runs adapter.Detect for every registered adapter so the
// wizard can pre-check the ones already present on disk. Detect is contractually
// side-effect-free, so this is cheap and safe to run during model setup.
func detectAdapters(adapters []agents.Adapter, env agents.Env) map[string]bool {
	out := make(map[string]bool, len(adapters))
	for _, a := range adapters {
		ok, _ := a.Detect(env)
		out[a.Name()] = ok
	}
	return out
}

// newInitWizardModel builds the wizard, seeded with the registered agents.
// Detected adapters start checked; Claude Code is always checked (it's the
// load-bearing adapter for hooks + skills, and unchecking it almost always
// reflects a mis-click, not intent).
func newInitWizardModel(rootPath string, registered []agents.Adapter, detected map[string]bool, defaults initDefaults) *initWizardModel {
	items := make([]tui.CheckItem, 0, len(registered))
	for _, a := range registered {
		name := a.Name()
		picked := detected[name]
		// Claude Code always defaults to picked — it owns the hook surface
		// and the CLAUDE.md global guidance; opting it out makes most other
		// agents pointless. The user can still uncheck it in the wizard.
		if name == "claude-code" {
			picked = true
		}
		items = append(items, tui.CheckItem{
			Label:  agentLabel(name),
			Detail: agentDetail(name),
			Picked: picked,
		})
	}
	cl := tui.NewChecklist(items)

	opts := tui.OptionsPanel{
		Toggles: []tui.Toggle{
			{
				Label:  "Claude Code hooks",
				Hint:   "PreToolUse + PreCompact + Stop",
				Detail: "redirect Read/Grep/Glob of indexed source through the graph",
				On:     defaults.hooks,
			},
			{
				Label:  "Codebase analysis",
				Hint:   "richer CLAUDE.md preamble (--analyze)",
				Detail: "index the repo once to embed a codebase overview into CLAUDE.md",
				On:     defaults.analyze,
			},
			{
				Label:  "Community skills",
				Hint:   "per-cluster routing under .claude/skills (--skills)",
				Detail: "auto-generate one SKILL.md per detected community",
				On:     defaults.skills,
			},
		},
		Selects: []tui.Select{
			{
				Label: "Hook posture",
				Options: []tui.SelectOption{
					{Value: "deny", Label: "deny", Detail: "PreToolUse refuses Read/Grep/Glob against indexed source"},
					{Value: "enrich", Label: "enrich", Detail: "PostToolUse appends graph context after the tool runs"},
					{Value: "consult-unlock", Label: "consult-unlock", Detail: "deny fallback reads until Gortex is queried once this session"},
					{Value: "nudge", Label: "nudge", Detail: "soft-deny once per burst, then let the next call proceed"},
				},
				Picked: hookModeIndex(defaults.hookMode),
			},
		},
	}

	// Install-only: append the anonymous-telemetry toggle as the last toggle
	// (kept last so the hooks/analyze/skills positional read-back is stable).
	if defaults.showTelemetry {
		opts.Toggles = append(opts.Toggles, tui.Toggle{
			Label:  "Anonymous telemetry",
			Hint:   "tool/command counts only — off by default",
			Detail: "no code, paths, or names; toggle later with `gortex telemetry on|off`",
			On:     defaults.telemetry,
		})
	}

	return &initWizardModel{
		step:          stepAgents,
		checklist:     cl,
		options:       opts,
		title:         "gortex init",
		rootPath:      rootPath,
		hooks:         defaults.hooks,
		hookMode:      defaults.hookMode,
		analyze:       defaults.analyze,
		skills:        defaults.skills,
		telemetry:     defaults.telemetry,
		showTelemetry: defaults.showTelemetry,
	}
}

// initDefaults bundles the boolean defaults the wizard inherits from CLI
// flags / global config. Resets are cheaper than threading a half-dozen
// scalars through newInitWizardModel.
type initDefaults struct {
	hooks    bool
	hookMode string
	analyze  bool
	skills   bool
	// telemetry seeds the (install-only) anonymous-telemetry toggle;
	// showTelemetry adds that toggle to the options panel. `gortex init`
	// leaves both zero so the toggle never appears there.
	telemetry     bool
	showTelemetry bool
}

// hookModeIndex maps a hook-mode value to its Select index, defaulting to
// "deny" (0) on an empty / unknown value.
func hookModeIndex(mode string) int {
	switch mode {
	case "enrich":
		return 1
	case "consult-unlock":
		return 2
	case "nudge":
		return 3
	default:
		return 0
	}
}

// wizardTickMsg paces the mesh animation at the same 80ms cadence as the
// dashboard so the brand mark feels consistent across screens.
type wizardTickMsg time.Time

func wizardTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return wizardTickMsg(t) })
}

// Init starts the mesh animation ticker.
func (m *initWizardModel) Init() tea.Cmd { return wizardTick() }

// Update handles keyboard input + ticks. Quit on q / esc / ctrl-c. Enter on
// stepConfirm sets m.confirmed and quits — the caller checks that flag to
// decide whether to proceed with runInit.
func (m *initWizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case wizardTickMsg:
		_ = msg
		m.tick++
		return m, wizardTick()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *initWizardModel) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global escape hatches first.
	switch k.String() {
	case "ctrl+c", "q", "esc":
		m.cancelled = true
		m.step = stepCancelled
		return m, tea.Quit
	}
	switch m.step {
	case stepAgents:
		return m.handleAgentsKey(k)
	case stepOptions:
		return m.handleOptionsKey(k)
	case stepConfirm:
		return m.handleConfirmKey(k)
	}
	return m, nil
}

func (m *initWizardModel) handleAgentsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "up", "k":
		m.checklist.Move(-1)
	case "down", "j":
		m.checklist.Move(1)
	case " ":
		m.checklist.ToggleCursor()
	case "a":
		m.checklist.PickAll()
	case "n":
		m.checklist.PickNone()
	case "i":
		m.checklist.PickInvert()
	case "enter":
		if m.checklist.PickedCount() == 0 {
			// Nothing picked — nudge by snapping to claude-code as the
			// load-bearing default rather than refusing to advance.
			for i := range m.checklist.Items {
				if m.checklist.Items[i].Label == agentLabel("claude-code") {
					m.checklist.Items[i].Picked = true
				}
			}
		}
		m.step = stepOptions
	}
	return m, nil
}

func (m *initWizardModel) handleOptionsKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "up", "k":
		m.options.Move(-1)
	case "down", "j":
		m.options.Move(1)
	case " ", "enter":
		// Space toggles toggles, advances selects, and on the last row falls
		// through to "next step" — but only when the cursor is on a Toggle.
		// Selects use ←/→.
		if !m.options.ToggleCursor() {
			// Select row: ←/→ behaviour; enter still advances when nothing
			// is highlighted to flip.
			if k.String() == "enter" {
				m.step = stepConfirm
			}
		}
	case "left", "h":
		m.options.CycleCursor(-1)
	case "right", "l":
		m.options.CycleCursor(1)
	case "tab", "N":
		m.step = stepConfirm
	case "backspace":
		m.step = stepAgents
	}
	return m, nil
}

func (m *initWizardModel) handleConfirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "enter", "y", "Y":
		m.collectChoices()
		m.confirmed = true
		return m, tea.Quit
	case "backspace", "b":
		m.step = stepOptions
	}
	return m, nil
}

// collectChoices pulls the wizard's pick state out of the widgets and into
// the flat fields runInit reads after the model exits. Called once on
// confirmation so the wizard's internal state can change without leaking.
func (m *initWizardModel) collectChoices() {
	m.pickedAgents = nil
	for _, it := range m.checklist.Items {
		if !it.Picked {
			continue
		}
		// Round-trip label → slug; preserve registration order.
		for slug, label := range agentLabels {
			if label == it.Label {
				m.pickedAgents = append(m.pickedAgents, slug)
				break
			}
		}
	}
	sort.Strings(m.pickedAgents)

	if len(m.options.Toggles) >= 3 {
		m.hooks = m.options.Toggles[0].On
		m.analyze = m.options.Toggles[1].On
		m.skills = m.options.Toggles[2].On
	}
	if m.showTelemetry && len(m.options.Toggles) >= 4 {
		m.telemetry = m.options.Toggles[3].On
	}
	if len(m.options.Selects) >= 1 {
		m.hookMode = m.options.Selects[0].Value()
	}
}

// View renders the current step. Layout: banner (always), step-line, body,
// hint footer. Each step's body is rendered by its own helper so the View
// stays a thin switch.
func (m *initWizardModel) View() string {
	if m.step == stepCancelled {
		return progress.StyleHint.Render("  cancelled — no changes made.\n")
	}

	title := m.title
	if title == "" {
		title = "gortex init"
	}
	banner := tui.Banner{
		Title:    title,
		Subtitle: subtitleForStep(m.step, m.rootPath),
		Tick:     m.tick,
	}.Render()

	body := ""
	hint := ""
	step := 0
	heading := ""
	switch m.step {
	case stepAgents:
		step, heading = 1, "Pick coding assistants"
		body = m.checklist.Render(10, 18)
		hint = tui.KeyHint("↑/↓ move", "space toggle", "a all · n none · i invert", "enter next", "q quit")
	case stepOptions:
		step, heading = 2, "Configure options"
		body = m.options.Render()
		hint = tui.KeyHint("↑/↓ row", "space/enter toggle", "←/→ cycle select", "tab next", "backspace prev")
	case stepConfirm:
		step, heading = 3, "Confirm"
		body = m.renderConfirmSummary()
		hint = tui.KeyHint("enter run", "backspace prev", "q quit")
	}

	stepLine := ""
	if heading != "" {
		stepLine = tui.StepLine(step, 3, heading)
	}

	parts := []string{banner, "", stepLine, "", body, "", hint}
	return lipgloss.JoinVertical(lipgloss.Left, parts...) + "\n"
}

func subtitleForStep(s wizardStep, root string) string {
	switch s {
	case stepAgents:
		// Use "this repo" when the path is "." or empty — the literal dot
		// reads awkwardly in the sentence ("wire ..").
		target := root
		if target == "" || target == "." {
			target = "this repo"
		}
		return "Welcome — let's wire " + target + "."
	case stepOptions:
		return "Tune the integration."
	case stepConfirm:
		return "Review and run."
	}
	return ""
}

// renderConfirmSummary draws a card-style block listing every choice so the
// user can verify before any disk write happens.
func (m *initWizardModel) renderConfirmSummary() string {
	var sb strings.Builder

	sb.WriteString(progress.Heading("agents", fmt.Sprintf("%d", m.checklist.PickedCount())))
	sb.WriteString("\n")
	picked := m.collectPickedLabels()
	if len(picked) == 0 {
		sb.WriteString(progress.StyleHint.Render("  (none — wizard will fall back to Claude Code)"))
	} else {
		sb.WriteString("  ")
		sb.WriteString(progress.Chips(picked, 64))
	}
	sb.WriteString("\n\n")

	sb.WriteString(progress.Heading("options"))
	sb.WriteString("\n")
	sb.WriteString(progress.Row("Hooks", chipBool(m.options.Toggles[0].On), 18))
	sb.WriteString("\n")
	sb.WriteString(progress.Row("Hook posture", m.options.Selects[0].Value(), 18))
	sb.WriteString("\n")
	sb.WriteString(progress.Row("Analyze repo", chipBool(m.options.Toggles[1].On), 18))
	sb.WriteString("\n")
	sb.WriteString(progress.Row("Generate skills", chipBool(m.options.Toggles[2].On), 18))
	sb.WriteString("\n\n")

	sb.WriteString(progress.StyleHint.Render("  press enter to apply these changes — files are written atomically and dry-run is available via --dry-run."))

	return sb.String()
}

func (m *initWizardModel) collectPickedLabels() []string {
	out := make([]string, 0, len(m.checklist.Items))
	for _, it := range m.checklist.Items {
		if it.Picked {
			out = append(out, it.Label)
		}
	}
	return out
}

func chipBool(on bool) string {
	if on {
		return progress.StyleOK.Render("on")
	}
	return progress.StyleHint.Render("off")
}

// cobraInitState carries the per-run inputs the wizard needs that aren't
// already on the package-level init* globals. Keeping it as a struct rather
// than passing scalars leaves room to grow without touching every callsite.
type cobraInitState struct {
	rootPath string
}

// runInitWizard launches the bubbletea wizard, blocks until the user either
// confirms or cancels, and (on confirmation) mutates the init* globals to
// reflect the wizard's picks. Returns cancelled=true when the user aborted
// (Ctrl-C / q / Esc) so the caller can short-circuit before any disk write.
//
// The wizard writes to cmd's stderr — alt-screen mode keeps the wizard's
// frame on its own surface, so when the user confirms the terminal scrollback
// is clean before the dashboard takes over.
func runInitWizard(cmd interface {
	ErrOrStderr() io.Writer
}, st *cobraInitState) (bool, error) {
	registry := buildRegistry()
	registered := registry.All()

	home, _ := osUserHomeDir()
	env := agents.Env{
		Root:         st.rootPath,
		Home:         home,
		Mode:         agents.ModeProject,
		InstallHooks: initInstallHooks,
		HookMode:     initHookMode,
	}
	detected := detectAdapters(registered, env)

	defaults := initDefaults{
		hooks:    initInstallHooks,
		hookMode: firstNonEmpty(initHookMode, "deny"),
		analyze:  initAnalyze,
		skills:   initSkills,
	}
	model := newInitWizardModel(st.rootPath, registered, detected, defaults)

	prog := tea.NewProgram(model,
		tea.WithOutput(cmd.ErrOrStderr()),
		tea.WithAltScreen(),
		tea.WithoutSignalHandler(),
	)
	finalModel, err := prog.Run()
	if err != nil {
		return false, fmt.Errorf("wizard: %w", err)
	}
	m, ok := finalModel.(*initWizardModel)
	if !ok || m.cancelled {
		return true, nil
	}
	if !m.confirmed {
		return true, nil
	}

	// Pour wizard picks into the init* globals so the orchestration code
	// downstream sees them as if they came from CLI flags.
	initInstallHooks = m.hooks
	initHookMode = m.hookMode
	initAnalyze = m.analyze
	initSkills = m.skills
	if len(m.pickedAgents) > 0 {
		initAgents = strings.Join(m.pickedAgents, ",")
	}
	wizardSelectedDashboard = true
	return false, nil
}

// osUserHomeDir and firstNonEmpty already live in daemon_status_tui.go so we
// reuse them. Kept here as a comment marker because grepping for the seam
// from this file is the more natural reading order.
