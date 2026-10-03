package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

// Service management — launchd on macOS, systemd --user on Linux.
//
// The service wraps `gortex daemon start` (foreground, without --detach)
// so the OS supervisor owns lifecycle. Users who prefer to manage the
// daemon manually keep using `gortex daemon start --detach`; the
// service integration is strictly opt-in via `install-service`.
//
// No root privileges. On both platforms the unit lives under the
// user's home so it starts with the user session and terminates on
// logout. System-wide installation (requires sudo) is a follow-up for
// shared-workstation deployments; not wired here.

var (
	daemonServiceName = "com.zzet.gortex" // launchd label + systemd unit base name
)

var daemonInstallServiceCmd = &cobra.Command{
	Use:   "install-service",
	Short: "Install a user-level launchd/systemd unit that keeps the daemon running",
	Long: `Writes a user-level service unit for the host OS (launchd on macOS,
systemd --user on Linux) that starts the daemon at login and restarts
it on crash. The service wraps 'gortex daemon start' in foreground mode
so the OS supervisor owns lifecycle; no --detach involved.

No root/sudo required — unit lives under your home directory.`,
	RunE: runDaemonInstallService,
}

var daemonUninstallServiceCmd = &cobra.Command{
	Use:   "uninstall-service",
	Short: "Remove the launchd/systemd unit and stop the daemon",
	RunE:  runDaemonUninstallService,
}

var daemonServiceStatusCmd = &cobra.Command{
	Use:   "service-status",
	Short: "Show whether the launchd/systemd unit is installed and active",
	RunE:  runDaemonServiceStatus,
}

func init() {
	daemonCmd.AddCommand(daemonInstallServiceCmd)
	daemonCmd.AddCommand(daemonUninstallServiceCmd)
	daemonCmd.AddCommand(daemonServiceStatusCmd)
}

// serviceEnvVar is a single environment entry rendered into a service
// unit. Render helpers escape Value for the target format.
type serviceEnvVar struct{ Key, Value string }

// xdgServiceEnv captures the XDG base-directory overrides in effect when
// the service unit is written, so the supervised daemon resolves the
// same config / data / cache locations as the shell that installed it.
//
// launchd and systemd --user start the daemon with a near-empty
// environment — they do NOT inherit the XDG_* variables a user exports
// from their shell or session manager. Without this capture the daemon
// falls back to ~/.gortex even though the user opted into an XDG layout
// (see internal/platform/xdg.go), silently splitting their state across
// two trees. Re-run install-service to re-capture changed values.
//
// Only absolute values are propagated — the rule platform.unifiedDir
// itself applies when honouring an override (a relative XDG path is
// ignored per the XDG Base Directory spec). XDG_RUNTIME_DIR is
// deliberately excluded: the init system sets it per session, so pinning
// an install-time value would point the daemon socket at a stale dir.
func xdgServiceEnv() []serviceEnvVar {
	var out []serviceEnvVar
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		if v := os.Getenv(name); v != "" && filepath.IsAbs(v) {
			out = append(out, serviceEnvVar{Key: name, Value: v})
		}
	}
	return out
}

// servicePath captures the installing shell's PATH so supervised language
// servers can be found outside the standard locations. Re-run install-service
// to re-capture changed values. Only absolute entries are kept: relative and
// empty entries would resolve against the service's working directory. An
// entry containing CR or LF is dropped, because it would split a systemd
// Environment= line. Entries need not exist yet; duplicates are removed without
// changing the shell's order.
// Missing defaults are appended for launchd; systemd keeps its default when the
// captured PATH is empty.
func servicePath(defaults []string) string {
	var entries []string
	for _, entry := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if filepath.IsAbs(entry) && !strings.ContainsAny(entry, "\r\n") && !slices.Contains(entries, entry) {
			entries = append(entries, entry)
		}
	}
	for _, entry := range defaults {
		if !slices.Contains(entries, entry) {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(os.PathListSeparator))
}

// xmlEscape renders s safe for an XML text node (the launchd plist) so a
// home path containing an XML metacharacter can't produce a malformed,
// unloadable plist.
func xmlEscape(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}

// systemdEnvValue renders a KEY=value assignment safe for a systemd
// Environment= line.
// `%` is escaped to `%%` because systemd treats it as a specifier
// introducer across the whole unit file (systemd.unit(5)) — an
// unescaped `%d` in a path would expand to a directory specifier and
// silently change the value the daemon sees. An assignment containing
// whitespace, a backslash or a quote is additionally double-quoted as a whole
// (with embedded quotes / backslashes escaped): systemd.syntax(7) recognizes
// a quote only at the start of an item, so `KEY="a b"` would not be unquoted,
// and systemd applies C-style escapes such as `\t` to unquoted text too.
// Plain paths (the common case) pass through unchanged.
func systemdEnvValue(v string) string {
	v = strings.ReplaceAll(v, "%", "%%")
	if !strings.ContainsAny(v, " \t\\\"'") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}

// runDaemonInstallService writes the unit file and enables + starts it.
// Existing daemon processes are stopped first so the service owns the
// only running instance after this returns.
func runDaemonInstallService(cmd *cobra.Command, _ []string) error {
	w := cmd.ErrOrStderr()

	// Stop any manual daemon that's currently running so the service
	// doesn't fight with it over the socket.
	if daemon.IsRunning() {
		fmt.Fprintln(w, "[gortex daemon] stopping existing daemon before install")
		// This is an internal lifecycle bounce — the supervisor starts the
		// daemon right after — not a user "stay down". Suppress the stop-intent
		// marker (as `daemon restart` does) so a failed install can't leave
		// autostart permanently disabled with no daemon and no service.
		daemonRestartActive = true
		err := runDaemonStop(cmd, nil)
		daemonRestartActive = false
		if err != nil {
			return fmt.Errorf("stop existing daemon: %w", err)
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(w, exe)
	case "linux":
		return installSystemd(w, exe)
	case "windows":
		return installWindowsTask(w, exe)
	default:
		return fmt.Errorf("service install is not supported on %s — run 'gortex daemon start --detach' to keep the daemon running in the background", runtime.GOOS)
	}
}

func runDaemonUninstallService(cmd *cobra.Command, _ []string) error {
	w := cmd.ErrOrStderr()
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd(w)
	case "linux":
		return uninstallSystemd(w)
	case "windows":
		return uninstallWindowsTask(w)
	default:
		return fmt.Errorf("service uninstall not supported on %s", runtime.GOOS)
	}
}

func runDaemonServiceStatus(cmd *cobra.Command, _ []string) error {
	w := cmd.OutOrStdout()
	switch runtime.GOOS {
	case "darwin":
		return statusLaunchd(w)
	case "linux":
		return statusSystemd(w)
	case "windows":
		return statusWindowsTask(w)
	default:
		return fmt.Errorf("service status not supported on %s", runtime.GOOS)
	}
}

// --- launchd (macOS) ------------------------------------------------------

// launchdPlistTemplate renders the LaunchAgent plist. KeepAlive uses the
// SuccessfulExit=false policy so the agent is restarted on a crash but NOT on a
// clean exit — the launchd analogue of systemd's Restart=on-failure, so an
// explicit `gortex daemon stop` (which exits 0) stays down instead of being
// resurrected by KeepAlive. RunAtLoad starts on login.
//
// StandardOutPath / StandardErrorPath redirect logs into the same file
// `gortex daemon logs` tails, so users don't need to remember two paths.
//
// EnvironmentVariables carries the installing shell's PATH with missing
// Homebrew / system defaults appended, plus any XDG_* overrides in effect at
// install time — see servicePath and xdgServiceEnv for the capture rules.
const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.Exe}}</string>
        <string>daemon</string>
        <string>start</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>{{.Path}}</string>
{{- range .EnvVars}}
        <key>{{.Key}}</key>
        <string>{{.Value}}</string>
{{- end}}
    </dict>
</dict>
</plist>
`

// renderLaunchdPlist fills launchdPlistTemplate. String values are
// XML-escaped so a path containing an XML metacharacter can't produce a
// malformed, unloadable plist.
func renderLaunchdPlist(label, exe, logPath string, env []serviceEnvVar) (string, error) {
	data := struct {
		Label, Exe, LogPath, Path string
		EnvVars                   []serviceEnvVar
	}{
		Label:   xmlEscape(label),
		Exe:     xmlEscape(exe),
		LogPath: xmlEscape(logPath),
		Path:    xmlEscape(servicePath([]string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin", "/bin"})),
		EnvVars: make([]serviceEnvVar, len(env)),
	}
	for i, e := range env {
		data.EnvVars[i] = serviceEnvVar{Key: e.Key, Value: xmlEscape(e.Value)}
	}
	var buf bytes.Buffer
	if err := template.Must(template.New("plist").Parse(launchdPlistTemplate)).Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", daemonServiceName+".plist"), nil
}

func installLaunchd(w io.Writer, exe string) error {
	path, err := launchdPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure LaunchAgents dir: %w", err)
	}

	plist, err := renderLaunchdPlist(daemonServiceName, exe, daemon.LogFilePath(), xdgServiceEnv())
	if err != nil {
		return fmt.Errorf("render plist: %w", err)
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	// -w persists the load across reboots; without it the service
	// starts only for the current login session.
	if err := runCmd(w, "launchctl", "load", "-w", path); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}
	fmt.Fprintf(w, "[gortex daemon] service installed at %s\n", path)
	fmt.Fprintf(w, "  logs: %s\n", daemon.LogFilePath())
	fmt.Fprintf(w, "  check: gortex daemon service-status\n")
	return nil
}

func uninstallLaunchd(w io.Writer) error {
	path, err := launchdPlistPath()
	if err != nil {
		return err
	}
	// unload is idempotent — emits a warning if the plist isn't loaded,
	// but exit 0. Swallow its error path so uninstall is safe to retry.
	_ = runCmd(w, "launchctl", "unload", "-w", path)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plist: %w", err)
	}
	fmt.Fprintln(w, "[gortex daemon] service uninstalled")
	return nil
}

// launchdDomains lists the launchd domains a user LaunchAgent can end up in,
// most likely first.
//
// `launchctl load` bootstraps the agent into the *calling* process's domain:
// a graphical login session puts it in gui/<uid>, an SSH or otherwise
// non-graphical session puts it in user/<uid>. Both are queried so the answer
// does not depend on which kind of session happens to be asking.
func launchdDomains(uid int) []string {
	return []string{fmt.Sprintf("gui/%d", uid), fmt.Sprintf("user/%d", uid)}
}

// launchdLookup is what one domain query established. The three-way split is
// the whole point: launchctl reports "this domain has no such service" and
// "I could not answer" with the same non-zero exit shape, and only the first
// entitles this command to say the service is not loaded.
type launchdLookup int

const (
	// launchdAbsent — launchctl answered, and the service is not in that domain.
	launchdAbsent launchdLookup = iota
	// launchdFound — launchctl answered and described the service.
	launchdFound
	// launchdUnreadable — the query itself failed, so nothing was established.
	// Reaching launchd needs the bootstrap port, and a context that cannot
	// get to it makes launchctl exit non-zero with no output at all — the
	// shape `launchctl list` returns under a seatbelt sandbox that denies
	// mach-lookup. `launchctl print` survives that particular sandbox, but
	// anything else that cannot reach launchd has to land here rather than be
	// mistaken for an absent service.
	launchdUnreadable
)

// launchdProbe asks launchd about one service path ("gui/501/com.zzet.gortex")
// and returns launchctl's combined output with its exit code. err is set only
// when launchctl could not be executed at all.
type launchdProbe func(servicePath string) (out string, exitCode int, err error)

// launchctl exit codes for the two negative answers. They are checked
// alongside the message text rather than instead of it, so a change to either
// one still leaves the other pointing at the same conclusion.
const (
	launchctlNoSuchDomain  = 112
	launchctlNoSuchService = 113
)

// launchctlPrintArgs builds the launchctl argv the probe runs. Split out so
// the domain-explicit form is pinned by a test without a launchd to talk to —
// the legacy `launchctl list <label>` this replaces searched only the caller's
// own domain, and that is the defect, so the argv is the fix.
func launchctlPrintArgs(servicePath string) []string {
	return []string{"print", servicePath}
}

// launchctlPrint is the production probe. `launchctl print <domain>/<label>`
// is domain-explicit, unlike the legacy `launchctl list <label>`, which only
// ever searches the caller's own domain.
func launchctlPrint(servicePath string) (string, int, error) {
	out, err := exec.Command("launchctl", launchctlPrintArgs(servicePath)...).CombinedOutput()
	if err == nil {
		return string(out), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode(), nil
	}
	return string(out), -1, err
}

// classifyLaunchctlPrint turns one probe result into a lookup outcome, plus a
// reason worth printing when the query failed.
//
// Anything unrecognised degrades to launchdUnreadable rather than to
// launchdAbsent. That is the safe direction: reporting "unknown" for a service
// that is genuinely gone costs the user one extra command, while reporting
// "not loaded" for a running one sends them to reload a healthy service.
func classifyLaunchctlPrint(out string, exitCode int, err error) (launchdLookup, string) {
	switch {
	case err != nil:
		return launchdUnreadable, fmt.Sprintf("could not run launchctl: %v", err)
	case exitCode == 0:
		return launchdFound, ""
	case strings.Contains(out, "Could not find service"),
		strings.Contains(out, "Could not find domain"),
		exitCode == launchctlNoSuchService,
		exitCode == launchctlNoSuchDomain:
		return launchdAbsent, ""
	}
	if detail := launchctlFailureDetail(out); detail != "" {
		return launchdUnreadable, fmt.Sprintf("launchctl exited %d: %s", exitCode, detail)
	}
	return launchdUnreadable, fmt.Sprintf(
		"launchctl exited %d with no output, so launchd could not be queried from this session", exitCode)
}

// launchctlFailureDetail picks the most informative line out of a failed
// launchctl run. "Bad request." is launchctl's own preamble and says nothing.
func launchctlFailureDetail(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "Bad request." {
			continue
		}
		return line
	}
	return ""
}

// launchdServiceState is the part of a `launchctl print` report a status line
// needs.
type launchdServiceState struct {
	State    string
	PID      string
	LastExit string
}

// parseLaunchctlPrint pulls those fields out of launchctl's nested, indented
// report. Only the first occurrence of each key is taken: the resource and
// jetsam coalition sub-blocks further down repeat `state = active`, and taking
// the last one would report a running daemon as something else entirely.
func parseLaunchctlPrint(out string) launchdServiceState {
	var st launchdServiceState
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			if st.State == "" {
				st.State = value
			}
		case "pid":
			if st.PID == "" {
				st.PID = value
			}
		case "last exit code":
			if st.LastExit == "" {
				st.LastExit = value
			}
		}
	}
	return st
}

// launchdServiceLoaded reports whether the LaunchAgent is bootstrapped into
// any of the user's launchd domains. It is the lifecycle-routing counterpart
// of reportLaunchdStatus, used to decide whether a supervisor owns the daemon.
//
// It collapses "absent" and "could not tell" into false on purpose. A caller
// choosing between the supervised and the manual path has to pick one: the
// manual path works whether or not a unit exists, while the supervised one
// hard-fails when there is nothing to drive. Guessing "supervised" from a
// query that failed would turn `daemon restart` into an error in exactly the
// restricted contexts where it works today.
func launchdServiceLoaded(uid int, probe launchdProbe) bool {
	for _, domain := range launchdDomains(uid) {
		out, code, err := probe(domain + "/" + daemonServiceName)
		if lookup, _ := classifyLaunchctlPrint(out, code, err); lookup == launchdFound {
			return true
		}
	}
	return false
}

func statusLaunchd(w io.Writer) error {
	path, err := launchdPlistPath()
	if err != nil {
		return err
	}
	return reportLaunchdStatus(w, path, os.Getuid(), launchctlPrint, daemon.IsRunning)
}

// reportLaunchdStatus renders the service state from evidence it can actually
// obtain. The probe and the daemon check are parameters so the whole decision
// table is testable without a Mac, a login session, or an installed service.
func reportLaunchdStatus(w io.Writer, plistPath string, uid int, probe launchdProbe, daemonRunning func() bool) error {
	switch _, err := os.Stat(plistPath); {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(w, "launchd: not installed (expected %s)\n", plistPath)
		return nil
	case err != nil:
		// Same discipline as the launchd query below: an unreadable path is
		// not evidence of an installed unit, and claiming one would put the
		// rest of this report on a foundation nothing established.
		fmt.Fprintf(w, "launchd: unknown — could not read %s (%v)\n", plistPath, err)
		return nil
	}
	fmt.Fprintf(w, "launchd: installed at %s\n", plistPath)

	domains := launchdDomains(uid)
	var unreadable []string
	for _, domain := range domains {
		out, code, err := probe(domain + "/" + daemonServiceName)
		switch lookup, reason := classifyLaunchctlPrint(out, code, err); lookup {
		case launchdFound:
			state := parseLaunchctlPrint(out)
			fmt.Fprintf(w, "status: loaded in %s\n", domain)
			for _, field := range []struct{ label, value string }{
				{"state", state.State},
				{"pid", state.PID},
				{"last exit code", state.LastExit},
			} {
				if field.value != "" {
					fmt.Fprintf(w, "  %s: %s\n", field.label, field.value)
				}
			}
			// A loaded unit that is not currently running says nothing about
			// whether a daemon is serving the user — one started outside
			// launchd looks identical from here, and reporting only launchd's
			// view of a stopped unit implies nothing is up.
			if state.State != "running" {
				noteRunningDaemon(w, daemonRunning)
			}
			return nil
		case launchdUnreadable:
			unreadable = append(unreadable, fmt.Sprintf("%s: %s", domain, reason))
		}
	}

	if len(unreadable) > 0 {
		// At least one domain could not be queried, so "not loaded" is not a
		// conclusion this command has earned — and neither is the advice that
		// follows from it.
		fmt.Fprintln(w, "status: unknown — launchd could not be queried")
		for _, line := range unreadable {
			fmt.Fprintln(w, "  "+line)
		}
	} else {
		fmt.Fprintf(w, "status: not loaded (checked %s)\n", strings.Join(domains, ", "))
		fmt.Fprintf(w, "  fix: launchctl load -w %s (or re-run `gortex daemon install-service`)\n", plistPath)
	}

	noteRunningDaemon(w, daemonRunning)
	return nil
}

// noteRunningDaemon adds the one fact left when launchd is not reporting a
// running service. The unit is only one of the ways the daemon runs, and
// stopping at launchd's answer would let this command imply nothing is running
// while a manually started daemon serves the user perfectly well.
func noteRunningDaemon(w io.Writer, daemonRunning func() bool) {
	if daemonRunning() {
		fmt.Fprintln(w, "  note: a gortex daemon is answering on its socket — see `gortex daemon status`")
	}
}

// --- systemd --user (Linux) ----------------------------------------------

// systemdUnitTemplate renders a user-level systemd service. Type=simple
// because `gortex daemon start` (without --detach) runs in the
// foreground; Restart=on-failure covers the crash-restart case without
// pounding on successful exits. Environment= lines carry a non-empty captured
// PATH and any XDG_* overrides in effect at install time so the supervised
// daemon resolves the same paths as the installing shell — see servicePath
// and xdgServiceEnv.
const systemdUnitTemplate = `[Unit]
Description=Gortex code intelligence daemon
Documentation=https://github.com/zzet/gortex
After=network.target

[Service]
Type=simple
ExecStart={{.Exe}} daemon start
{{- range .EnvVars}}
Environment={{.}}
{{- end}}
Restart=on-failure
RestartSec=2
StandardOutput=append:{{.LogPath}}
StandardError=append:{{.LogPath}}

[Install]
WantedBy=default.target
`

// renderSystemdUnit fills systemdUnitTemplate, quoting Environment=
// assignments that need it.
func renderSystemdUnit(exe, logPath string, env []serviceEnvVar) (string, error) {
	if path := servicePath(nil); path != "" {
		env = append([]serviceEnvVar{{Key: "PATH", Value: path}}, env...)
	}
	data := struct {
		Exe, LogPath string
		EnvVars      []string
	}{Exe: exe, LogPath: logPath, EnvVars: make([]string, len(env))}
	for i, e := range env {
		data.EnvVars[i] = systemdEnvValue(e.Key + "=" + e.Value)
	}
	var buf bytes.Buffer
	if err := template.Must(template.New("unit").Parse(systemdUnitTemplate)).Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func systemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", daemonServiceName+".service"), nil
}

func installSystemd(w io.Writer, exe string) error {
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("ensure systemd user dir: %w", err)
	}

	unit, err := renderSystemdUnit(exe, daemon.LogFilePath(), xdgServiceEnv())
	if err != nil {
		return fmt.Errorf("render unit: %w", err)
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	if err := runCmd(w, "systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	// `enable --now` = enable for autostart + start immediately. Bundled
	// so the user gets a running service in one command.
	if err := runCmd(w, "systemctl", "--user", "enable", "--now", daemonServiceName); err != nil {
		return fmt.Errorf("systemctl enable --now: %w", err)
	}
	fmt.Fprintf(w, "[gortex daemon] service installed at %s\n", path)
	fmt.Fprintf(w, "  logs: %s (or `journalctl --user -u %s -f`)\n", daemon.LogFilePath(), daemonServiceName)
	fmt.Fprintf(w, "  check: gortex daemon service-status\n")
	return nil
}

func uninstallSystemd(w io.Writer) error {
	// `disable --now` = stop + disable autostart. Swallowed so uninstall
	// is safe to run on a never-installed system.
	_ = runCmd(w, "systemctl", "--user", "disable", "--now", daemonServiceName)

	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unit: %w", err)
	}
	_ = runCmd(w, "systemctl", "--user", "daemon-reload")
	fmt.Fprintln(w, "[gortex daemon] service uninstalled")
	return nil
}

func statusSystemd(w io.Writer) error {
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(w, "systemd: not installed (expected %s)\n", path)
		return nil
	}
	fmt.Fprintf(w, "systemd: installed at %s\n", path)
	out, err := exec.Command("systemctl", "--user", "is-active", daemonServiceName).CombinedOutput()
	status := strings.TrimSpace(string(out))
	fmt.Fprintf(w, "status: %s", status)
	if err != nil {
		// is-active exits 3 for "inactive" — not really a CLI error.
		fmt.Fprintln(w)
		return nil
	}
	fmt.Fprintln(w)
	return nil
}

// runCmd is a helper that runs an external command with its stdout /
// stderr piped through the caller's writer so users see launchctl /
// systemctl output in context with our own.
func runCmd(w io.Writer, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = w
	c.Stderr = w
	return c.Run()
}

// --- Task Scheduler (Windows) --------------------------------------------
//
// Windows has no user-level unit file the way launchd / systemd --user do,
// and a true Windows Service would require admin rights plus Service Control
// Manager integration in the daemon. The per-user analogue that keeps the
// no-admin, autostart-at-login contract is a Task Scheduler logon task, driven
// through schtasks: it runs `gortex daemon start` at each logon under the
// current limited user. (schtasks' CLI flags don't expose restart-on-failure —
// that's a Task-Scheduler-XML-only setting — so crash-restart parity with
// launchd KeepAlive / systemd Restart=on-failure is a follow-up.)

// windowsTaskName is the Task Scheduler task that autostarts the daemon at
// logon on Windows.
const windowsTaskName = "GortexDaemon"

// windowsTaskCreateArgs builds the schtasks argv that registers the logon
// autostart task. Split out so the arg vector is unit-testable without a
// Windows host: the task runs `"<exe>" daemon start` at each logon (ONLOGON)
// under the current limited user (RL LIMITED), replacing any existing task (F).
func windowsTaskCreateArgs(taskName, exe string) []string {
	return []string{
		"/Create",
		"/TN", taskName,
		"/TR", fmt.Sprintf(`"%s" daemon start`, exe),
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F",
	}
}

func installWindowsTask(w io.Writer, exe string) error {
	if err := runCmd(w, "schtasks", windowsTaskCreateArgs(windowsTaskName, exe)...); err != nil {
		return fmt.Errorf("schtasks create: %w", err)
	}
	// Start it now so the user gets a running daemon in one command — the
	// Windows analogue of systemd `enable --now` / launchd RunAtLoad.
	_ = runCmd(w, "schtasks", "/Run", "/TN", windowsTaskName)
	fmt.Fprintf(w, "[gortex daemon] logon task installed (%s)\n", windowsTaskName)
	fmt.Fprintf(w, "  logs: %s\n", daemon.LogFilePath())
	fmt.Fprintf(w, "  check: gortex daemon service-status\n")
	return nil
}

func uninstallWindowsTask(w io.Writer) error {
	// End a running instance, then delete the task. Both are swallowed so
	// uninstall is idempotent on a host where the task was never installed
	// (schtasks exits non-zero for an unknown task).
	_ = runCmd(w, "schtasks", "/End", "/TN", windowsTaskName)
	_ = runCmd(w, "schtasks", "/Delete", "/TN", windowsTaskName, "/F")
	fmt.Fprintln(w, "[gortex daemon] logon task uninstalled")
	return nil
}

func statusWindowsTask(w io.Writer) error {
	// `schtasks /Query /TN <name>` exits 0 when the task exists, non-zero
	// otherwise.
	out, err := exec.Command("schtasks", "/Query", "/TN", windowsTaskName).CombinedOutput()
	if err != nil {
		fmt.Fprintf(w, "windows task: not installed (%s)\n", windowsTaskName)
		return nil
	}
	fmt.Fprintf(w, "windows task: installed (%s)\n", windowsTaskName)
	// Surface the Status column if schtasks printed one.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Status:") {
			fmt.Fprintln(w, "  "+line)
		}
	}
	return nil
}
