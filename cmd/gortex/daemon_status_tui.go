package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/progress"
)

var osUserHomeDir = os.UserHomeDir

// ---- styles ------------------------------------------------------------

var (
	tuiAccent      = lipgloss.NewStyle().Foreground(progress.ColorAccent)
	tuiTitle       = lipgloss.NewStyle().Bold(true).Foreground(progress.ColorFg)
	tuiSub         = lipgloss.NewStyle().Foreground(progress.ColorFgDim)
	tuiHeading     = lipgloss.NewStyle().Bold(true).Foreground(progress.ColorFgDim)
	tuiCount       = lipgloss.NewStyle().Foreground(progress.ColorMuted)
	tuiKey         = lipgloss.NewStyle().Bold(true).Foreground(progress.ColorFg)
	tuiVal         = lipgloss.NewStyle().Foreground(progress.ColorFgDim)
	tuiSelected    = lipgloss.NewStyle().Foreground(progress.ColorAccent).Bold(true)
	tuiHint        = lipgloss.NewStyle().Foreground(progress.ColorMuted)
	tuiErrInline   = lipgloss.NewStyle().Foreground(progress.ColorErr)
	tuiKeybindKey  = lipgloss.NewStyle().Bold(true).Foreground(progress.ColorFg)
	tuiKeybindHint = lipgloss.NewStyle().Foreground(progress.ColorMuted)
)

// ---- repo item / delegate ---------------------------------------------

type repoItem struct {
	name      string
	workspace string
	memory    string
	memBytes  uint64
	files     int
	nodes     int
	edges     int
	path      string
	// state names an inventory problem — "MISSING — path deleted" or
	// "not indexed" — and replaces the counts cell when set. A repo
	// whose directory is gone renders as a row of zeros otherwise,
	// which reads as an empty repo rather than an absent one (#312).
	state string
}

func (r repoItem) FilterValue() string {
	// Concatenate every searchable field so users can match by name,
	// workspace, path fragment, or inventory state (`/MISSING`) via the
	// bubbles list's `/` filter.
	return r.name + " " + r.workspace + " " + r.path + " " + r.state
}

type repoDelegate struct{ width int }

func (d repoDelegate) Height() int                         { return 1 }
func (d repoDelegate) Spacing() int                        { return 0 }
func (d repoDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }
func (d repoDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	r, ok := listItem.(repoItem)
	if !ok {
		return
	}

	cursor := "  "
	style := tuiVal
	if index == m.Index() {
		cursor = tuiAccent.Render("▸ ")
		style = tuiSelected
	}

	// Adaptive column widths: drop the path column entirely under ~120 cols.
	nameW := 26
	wsW := 18
	memW := 10
	statsW := 32
	pathOK := d.width >= 120
	pathW := 0
	if pathOK {
		pathW = d.width - (2 + nameW + 2 + wsW + 2 + memW + 3 + statsW + 3)
		if pathW < 20 {
			pathW = 20
		}
	}

	stats := fmt.Sprintf("%4df · %9sn · %9se", r.files, humanizeInt(r.nodes), humanizeInt(r.edges))
	if r.state != "" {
		// Padded to the counts cell's natural width so the path column
		// below stays aligned with every other row.
		stats = fmt.Sprintf("%-31s", r.state)
	}
	row := fmt.Sprintf("%-*s  %-*s  %*s   %s",
		nameW, truncate(r.name, nameW),
		wsW, truncate(r.workspace, wsW),
		memW, r.memory,
		stats,
	)
	if pathOK {
		row += "   " + tuiHint.Render(truncate(shortenPath(r.path), pathW))
	}

	fmt.Fprint(w, cursor+style.Render(row))
}

// repoItemState renders a tracked repo's inventory problem for the watch
// TUI, or "" for a healthy repo (which shows its counts instead).
func repoItemState(r daemon.TrackedRepoStatus) string {
	switch {
	case r.Missing:
		return "MISSING — path deleted"
	case r.Unloaded:
		return "not indexed"
	case repoIndexIsEmpty(r):
		return "EMPTY — 0 files indexed"
	default:
		return ""
	}
}

// ---- model -------------------------------------------------------------

type tuiTickMsg time.Time
type tuiStatusMsg daemon.StatusResponse
type tuiFetchErrMsg struct{ err error }

type statusTUI struct {
	interval time.Duration
	width    int
	height   int

	status     daemon.StatusResponse
	hasStatus  bool
	err        error
	lastUpdate time.Time
	meshTick   int

	repos list.Model
}

func newStatusTUI(interval time.Duration) statusTUI {
	delegate := repoDelegate{}
	l := list.New([]list.Item{}, delegate, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(true)
	l.SetShowHelp(false)
	l.SetShowPagination(true)
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()
	l.Styles.NoItems = tuiHint
	l.Styles.StatusBar = tuiHint
	l.Styles.StatusEmpty = tuiHint
	l.Styles.PaginationStyle = tuiHint
	l.Styles.HelpStyle = tuiHint
	l.SetStatusBarItemName("repo", "repos")
	return statusTUI{interval: interval, repos: l}
}

func (m statusTUI) Init() tea.Cmd {
	return tea.Batch(
		tuiFetchCmd(),
		tea.Tick(m.interval, func(t time.Time) tea.Msg { return tuiTickMsg(t) }),
		tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg { return tuiMeshTickMsg(t) }),
	)
}

type tuiMeshTickMsg time.Time

func tuiFetchCmd() tea.Cmd {
	return func() tea.Msg {
		st, err := fetchDaemonStatusForCLI()
		if err != nil {
			return tuiFetchErrMsg{err: err}
		}
		return tuiStatusMsg(st)
	}
}

func (m statusTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.repos.SetSize(m.reposListWidth(), m.reposListHeight())
		// Update the delegate's width so each row knows whether to draw the path column.
		m.repos.SetDelegate(repoDelegate{width: m.reposListWidth()})

	case tuiTickMsg:
		return m, tea.Batch(
			tuiFetchCmd(),
			tea.Tick(m.interval, func(t time.Time) tea.Msg { return tuiTickMsg(t) }),
		)

	case tuiMeshTickMsg:
		m.meshTick++
		return m, tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg { return tuiMeshTickMsg(t) })

	case tuiStatusMsg:
		m.status = daemon.StatusResponse(msg)
		m.hasStatus = true
		m.err = nil
		m.lastUpdate = time.Now()
		items := make([]list.Item, 0, len(m.status.TrackedRepos))
		for _, r := range m.status.TrackedRepos {
			items = append(items, repoItem{
				name:      firstNonEmpty(r.Prefix, r.Name),
				workspace: workspaceLabel(r),
				memory:    humanizeBytes(r.Memory.TotalBytes),
				memBytes:  r.Memory.TotalBytes,
				files:     r.Files,
				nodes:     r.Nodes,
				edges:     r.Edges,
				path:      r.Path,
				state:     repoItemState(r),
			})
		}
		// Sort by total memory desc — biggest repos first, like top.
		sort.Slice(items, func(i, j int) bool {
			a, b := items[i].(repoItem), items[j].(repoItem)
			if a.memBytes != b.memBytes {
				return a.memBytes > b.memBytes
			}
			return a.name < b.name
		})
		m.repos.SetItems(items)
		m.repos.SetSize(m.reposListWidth(), m.reposListHeight())

	case tuiFetchErrMsg:
		m.err = msg.err
		m.lastUpdate = time.Now()

	case tea.KeyMsg:
		// Don't intercept keys while the list is in filter mode — user is
		// typing the query.
		if m.repos.FilterState() == list.Filtering {
			break
		}
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	m.repos, cmd = m.repos.Update(msg)
	return m, cmd
}

func (m statusTUI) View() string {
	if m.width == 0 {
		return ""
	}
	// Inset every section by tuiPadding columns and weave explicit blank
	// lines between sections so the mesh logo and section blocks have
	// clear breathing room. Empty sections (e.g. no sessions yet) are
	// elided along with their trailing blank.
	var lines []string
	push := func(s string) {
		if s != "" {
			lines = append(lines, indentBlock(s, tuiPadding))
		}
	}
	gap := func(n int) {
		for i := 0; i < n; i++ {
			lines = append(lines, "")
		}
	}

	gap(1)               // top breathing room
	push(m.renderHeader())
	gap(2)               // logo gets extra space below
	if w := m.renderWorkspaces(); w != "" {
		push(w)
		gap(1)
	}
	push(m.renderRepos())
	gap(1)
	if s := m.renderSessions(); s != "" {
		push(s)
		gap(1)
	}
	if s := m.renderServers(); s != "" {
		push(s)
		gap(1)
	}
	if s := m.renderLSPRouter(); s != "" {
		push(s)
		gap(1)
	}
	push(m.renderFooter())
	return strings.Join(lines, "\n")
}

// tuiPadding is the left inset (in columns) applied to every section.
const tuiPadding = 2

// indentBlock prefixes every non-empty line of s with n spaces.
func indentBlock(s string, n int) string {
	if s == "" {
		return s
	}
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = pad + line
	}
	return strings.Join(lines, "\n")
}

// ---- layout sections ---------------------------------------------------

func (m statusTUI) renderHeader() string {
	mesh := miniMesh(m.meshTick)
	right := m.headerRightCol()
	// Give the logo room to breathe — 4 columns of gap, vertically centered
	// on the right column so the title line sits on the second mesh row.
	right = "\n" + right // shift down one row so "gortex daemon" lines up with the mesh middle
	return lipgloss.JoinHorizontal(lipgloss.Top, mesh, "    ", right)
}

func (m statusTUI) headerRightCol() string {
	if m.err != nil && !m.hasStatus {
		return lipgloss.JoinVertical(
			lipgloss.Left,
			tuiTitle.Render("gortex daemon"),
			"",
			tuiErrInline.Render("✗ "+m.err.Error()),
			tuiHint.Render("retrying every "+m.interval.String()),
		)
	}
	if !m.hasStatus {
		return lipgloss.JoinVertical(
			lipgloss.Left,
			tuiTitle.Render("gortex daemon"),
			"",
			tuiHint.Render("connecting…"),
		)
	}
	st := m.status
	state := tuiAccent.Render("ready")
	if !st.Ready {
		state = lipgloss.NewStyle().Foreground(progress.ColorWarn).Render("warming")
	}
	row1 := tuiTitle.Render("gortex daemon")
	row2 := strings.Join([]string{
		tuiSub.Render(versionString(st.Version)),
		tuiSub.Render("pid " + fmt.Sprintf("%d", st.PID)),
		state,
		tuiSub.Render("up " + humanizeDur(time.Duration(st.UptimeSeconds)*time.Second)),
	}, tuiHint.Render(" · "))
	row3Cells := []string{
		tuiKey.Render(humanizeBytes(st.Runtime.Alloc)) + tuiSub.Render(" alloc"),
	}
	// Omitted entirely when the backend has no real count to report —
	// its only figure would be a since-construction Add/Remove delta,
	// which is not a document count and can be negative.
	if st.SearchBackend.DocCountKnown {
		row3Cells = append(row3Cells,
			tuiKey.Render(humanizeInt(st.SearchBackend.DocCount))+tuiSub.Render(" docs"))
	}
	row3Cells = append(row3Cells,
		tuiKey.Render(fmt.Sprintf("%d", st.Sessions))+tuiSub.Render(" sessions"),
		tuiKey.Render(fmt.Sprintf("%d", st.Runtime.NumGoroutine))+tuiSub.Render(" goroutines"),
	)
	row3 := strings.Join(row3Cells, tuiHint.Render(" · "))
	return lipgloss.JoinVertical(lipgloss.Left, row1, "", row2, row3)
}

func (m statusTUI) renderWorkspaces() string {
	if !m.hasStatus || len(m.status.Workspaces) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, tuiHeading.Render("workspaces")+"   "+tuiCount.Render(fmt.Sprintf("· %d", len(m.status.Workspaces))))
	for _, ws := range m.status.Workspaces {
		projects := ""
		if len(ws.Projects) > 0 {
			projects = "  " + tuiHint.Render(strings.Join(ws.Projects, " · "))
		}
		stats := fmt.Sprintf("%2d %s · %s files · %s nodes · %s edges",
			len(ws.Repos), pluralize(len(ws.Repos), "repo", "repos"),
			humanizeInt(ws.Files), humanizeInt(ws.Nodes), humanizeInt(ws.Edges))
		line := "  " + tuiKey.Render(padRight(ws.Slug, 12)) + "  " + tuiVal.Render(stats) + projects
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m statusTUI) renderRepos() string {
	if !m.hasStatus {
		return ""
	}
	header := tuiHeading.Render("repos") + "   " + tuiCount.Render(fmt.Sprintf("· %d", len(m.status.TrackedRepos)))
	hint := tuiKeybindKey.Render("/") + tuiKeybindHint.Render(" filter   ") +
		tuiKeybindKey.Render("↑↓") + tuiKeybindHint.Render(" navigate")
	row := lipgloss.JoinHorizontal(
		lipgloss.Top,
		header,
		strings.Repeat(" ", maxInt(2, m.width-lipgloss.Width(header)-lipgloss.Width(hint))),
		hint,
	)
	return row + "\n" + m.repos.View()
}

func (m statusTUI) renderSessions() string {
	if !m.hasStatus || len(m.status.MCPSessions) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, tuiHeading.Render("sessions")+"   "+tuiCount.Render(fmt.Sprintf("· %d", len(m.status.MCPSessions))))
	for _, s := range m.status.MCPSessions {
		client := s.ClientName
		if client == "" {
			client = "(unknown)"
		}
		if s.ClientVersion != "" {
			client += " " + tuiHint.Render(s.ClientVersion)
		}
		dur := humanizeDur(time.Duration(s.ConnectedSecs) * time.Second)
		shortID := truncate(s.ID, 16)
		cwd := shortenPath(s.Cwd)
		line := "  " + tuiKey.Render(padRight(shortID, 16)) + "  " +
			tuiVal.Render(padRight(client, 22)) + "  " +
			tuiSub.Render(padRight(dur, 7)) + "  " +
			tuiHint.Render(cwd)
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m statusTUI) renderServers() string {
	if !m.hasStatus || len(m.status.ConfiguredServers) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, tuiHeading.Render("servers")+"   "+tuiCount.Render(fmt.Sprintf("· %d", len(m.status.ConfiguredServers))))
	for _, s := range m.status.ConfiguredServers {
		var flags []string
		if s.Local {
			flags = append(flags, "local")
		}
		if s.Default {
			flags = append(flags, "default")
		}
		if s.HasAuth {
			flags = append(flags, "auth")
		}
		if len(s.Workspaces) > 0 {
			flags = append(flags, "ws="+strings.Join(s.Workspaces, ","))
		}
		flagLine := ""
		if len(flags) > 0 {
			flagLine = "   " + tuiHint.Render(strings.Join(flags, " · "))
		}
		line := "  " + tuiKey.Render(padRight(s.Slug, 12)) + "  " + tuiVal.Render(s.URL) + flagLine
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m statusTUI) renderLSPRouter() string {
	if !m.hasStatus || m.status.LSPRouter == nil {
		return ""
	}
	r := m.status.LSPRouter
	specs := r.EnabledSpecs
	active := r.ActiveProviders
	if len(specs) == 0 && len(active) == 0 {
		return ""
	}
	var lines []string
	header := tuiHeading.Render("lsp router")
	header += "   " + tuiCount.Render(fmt.Sprintf("· %d enabled · %d alive", len(specs), len(active)))
	lines = append(lines, header)
	for _, s := range specs {
		state := "·"
		stateRender := tuiSub
		if s.Available {
			state = "✓"
			stateRender = tuiAccent
		}
		line := "  " + stateRender.Render(state) + " " +
			tuiKey.Render(padRight(s.Name, 28)) + "  " +
			tuiVal.Render(padRight(s.Languages, 28))
		lines = append(lines, line)
	}
	for _, a := range active {
		lines = append(lines, "  "+tuiHint.Render("alive: "+a.Spec+"@"+a.Workspace+"  last_used="+a.LastUsed))
	}
	return strings.Join(lines, "\n")
}

func (m statusTUI) renderFooter() string {
	left := tuiHint.Render(time.Now().Format("15:04:05") + " · refreshing every " + m.interval.String())
	right := strings.Join([]string{
		tuiKeybindKey.Render("/") + tuiKeybindHint.Render(" filter"),
		tuiKeybindKey.Render("↑↓") + tuiKeybindHint.Render(" scroll"),
		tuiKeybindKey.Render("q") + tuiKeybindHint.Render(" quit"),
	}, tuiKeybindHint.Render("  "))
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}
	return left + strings.Repeat(" ", pad) + right
}

// ---- sizing -----------------------------------------------------------

func (m statusTUI) reposListWidth() int {
	w := m.width - 2
	if w < 40 {
		w = 40
	}
	return w
}

func (m statusTUI) reposListHeight() int {
	if m.height == 0 {
		return 10
	}
	// Mirror the layout in View(): top gap (1) + header (5) + gap-below-logo
	// (2) + each non-empty section's heading + rows + 1 gap line + repos
	// heading (1) + list status/pagination (2) + footer (1) + small slack.
	reserved := 1 + 5 + 2
	if m.hasStatus {
		if len(m.status.Workspaces) > 0 {
			reserved += 1 + len(m.status.Workspaces) + 1
		}
		if len(m.status.MCPSessions) > 0 {
			reserved += 1 + len(m.status.MCPSessions) + 1
		}
		if len(m.status.ConfiguredServers) > 0 {
			reserved += 1 + len(m.status.ConfiguredServers) + 1
		}
	}
	reserved += 1 // repos heading row
	reserved += 2 // bubbles list status bar + pagination
	reserved += 1 // gap below repos
	reserved += 1 // footer
	h := m.height - reserved
	if h < 6 {
		h = 6
	}
	return h
}

// ---- helpers ----------------------------------------------------------

// miniMesh returns a 5-line mesh logo with the active node walking once per
// tick, sized for the header (no label/sub — just the mark).
func miniMesh(tick int) string {
	const reset = "\x1b[0m"
	cAccent := "\x1b[38;2;95;214;122m"
	cPerim := "\x1b[38;2;240;240;240m"
	cInner := "\x1b[38;2;70;70;78m"

	perimeter := [12][2]int{
		{0, 1}, {0, 2}, {0, 3},
		{1, 4}, {2, 4}, {3, 4},
		{4, 3}, {4, 2}, {4, 1},
		{3, 0}, {2, 0}, {1, 0},
	}
	innerAccents := map[[2]int]bool{
		{2, 2}: true,
		{2, 3}: true,
	}
	cellAt := func(r, c int) int {
		corner := (r == 0 || r == 4) && (c == 0 || c == 4)
		if corner {
			return 0
		}
		if r == 0 || r == 4 || c == 0 || c == 4 {
			return 1
		}
		if innerAccents[[2]int{r, c}] {
			return 1
		}
		return 2
	}

	active := perimeter[tick%len(perimeter)]
	var lines [5]string
	for r := 0; r < 5; r++ {
		var b strings.Builder
		for c := 0; c < 5; c++ {
			switch cellAt(r, c) {
			case 0:
				b.WriteString(" ")
			case 1:
				if r == active[0] && c == active[1] {
					b.WriteString(cAccent + "●" + reset)
				} else {
					b.WriteString(cPerim + "●" + reset)
				}
			case 2:
				b.WriteString(cInner + "·" + reset)
			}
			if c < 4 {
				b.WriteString(" ")
			}
		}
		lines[r] = b.String()
	}
	return strings.Join(lines[:], "\n")
}

func humanizeBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	v := float64(b) / float64(div)
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp]
	return fmt.Sprintf("%.1f %s", v, suffix)
}

func humanizeDur(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}

func truncate(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	// Naive byte truncation works fine for the ASCII-heavy fields here;
	// when content is multibyte (paths, names with Unicode), the trailing
	// ellipsis covers any visual jitter.
	return s[:n-1] + "…"
}

func padRight(s string, n int) string {
	w := lipgloss.Width(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

func shortenPath(p string) string {
	home := homeDir()
	if home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

func homeDir() string {
	if h, err := userHomeDir(); err == nil {
		return h
	}
	return ""
}

// userHomeDir is a thin alias so tests can stub it without pulling in
// os in this file's import set.
var userHomeDir = osUserHomeDir

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// versionString prefixes the version with "v" only when the string isn't
// already prefixed; the daemon may report "0.19.1" or "v0.19.1" depending on
// how the binary was built.
func versionString(v string) string {
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "v") || strings.HasPrefix(v, "V") {
		return v
	}
	return "v" + v
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func workspaceLabel(r daemon.TrackedRepoStatus) string {
	if r.Workspace == "" {
		return ""
	}
	if r.WorkspaceProject != "" && r.WorkspaceProject != r.Workspace {
		return r.Workspace + "/" + r.WorkspaceProject
	}
	return r.Workspace
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
