package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/doctor"
)

// doctorCmd implements `gortex doctor`. It re-runs every adapter's
// Detect + Plan against the current environment and reports drift —
// files we'd like to write, files that already exist, files whose
// contents look wrong — and then goes past what a config file can
// prove: whether the hooks those files declare have actually run,
// whether the agent is calling Gortex tools, and what the savings
// ledger recorded. Zero writes: doctor never modifies disk.
//
// The runtime half exists because an agent config is not evidence.
// Codex, for one, records trust against each hook's hash and skips new
// or changed hooks until the user approves them — so a fully populated
// config.toml and a completely inert integration look identical on
// disk. Only the invocation log tells them apart.
//
// This is the zero-op form of the installer and the primary
// support-channel tool. Output defaults to a human-readable table;
// --json emits a machine-readable report for CI consumers.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose the Gortex install: config, hook activity, agent adoption, savings",
	Long: `Reports the machine-wide state of every Gortex integration without
writing anything.

Static half — per-adapter Detect + Plan: which files exist, which are
missing, whether an MCP stanza is stale.

Runtime half — the evidence a config file cannot carry:

  hook activity   did the hook processes actually run, did they inject
                  context, did they reach the daemon? An event configured
                  with zero invocations is the signature of a hook the
                  agent is skipping (Codex requires per-hook trust via
                  /hooks before a new or changed hook runs).
  adoption        for agents that keep session transcripts, how often the
                  model called a Gortex tool versus shelling out.
  savings         what the ledger recorded, and how much of it carries no
                  client or model attribution.

Examples:

  gortex doctor                    # last 7 days
  gortex doctor --days 30
  gortex doctor --redact           # hash repo paths before sharing
  gortex doctor --json             # machine-readable

Exit status is 1 when a blocker is found, so CI can gate on it.`,
	Args: cobra.NoArgs,
	RunE: runDoctor,
}

// initDoctorCmd keeps `gortex init doctor` working for anyone with it in a
// script. The name always oversold its scope — doctor reports machine-wide
// state, not this repo's — so the canonical spelling is `gortex doctor`.
var initDoctorCmd = &cobra.Command{
	Use:        "doctor",
	Short:      "Deprecated alias for `gortex doctor`",
	Args:       cobra.NoArgs,
	Deprecated: "use `gortex doctor` (doctor reports machine-wide state, not one repo's).",
	RunE:       runDoctor,
}

var (
	doctorJSON   bool
	doctorDays   int
	doctorRedact bool
	doctorAll    bool
)

func init() {
	for _, c := range []*cobra.Command{doctorCmd, initDoctorCmd} {
		c.Flags().BoolVar(&doctorJSON, "json", false, "emit a structured JSON report on stdout")
		c.Flags().IntVar(&doctorDays, "days", 7, "look-back window for hook activity, adoption, and savings")
		c.Flags().BoolVar(&doctorRedact, "redact", false, "rewrite repo and home paths, and hash branch names, so the report can be shared")
		c.Flags().BoolVar(&doctorAll, "all", false, "list planned files for adapters that are not installed too")
	}
	silenceDoctorErrors(doctorCmd, initDoctorCmd)
	rootCmd.AddCommand(doctorCmd)
	initCmd.AddCommand(initDoctorCmd)
}

// DoctorEnvironment captures the live wiring doctor can verify because
// Gortex owns the binary and the daemon — something a config-file-only
// integration cannot check: that the `gortex` the editor will launch is the
// one on PATH, and that the daemon it proxies to actually answers a handshake.
type DoctorEnvironment struct {
	BinaryOnPath       bool   `json:"binary_on_path"`
	BinaryPath         string `json:"binary_path,omitempty"`
	BinaryError        string `json:"binary_error,omitempty"`
	CLIVersion         string `json:"cli_version,omitempty"`
	DaemonRunning      bool   `json:"daemon_running"`
	DaemonSocket       string `json:"daemon_socket,omitempty"`
	DaemonVersion      string `json:"daemon_version,omitempty"`
	DaemonError        string `json:"daemon_error,omitempty"`
	VersionSkewWarning string `json:"version_skew_warning,omitempty"`
}

// DoctorAgentReport is one agent's slice of the doctor output.
// Each planned file shows up in Files along with its observed
// status on disk (present, missing, …).
type DoctorAgentReport struct {
	Name       string             `json:"name"`
	DocsURL    string             `json:"docs_url,omitempty"`
	Detected   bool               `json:"detected"`
	Configured bool               `json:"configured"`
	Files      []DoctorFileStatus `json:"files,omitempty"`
}

// DoctorFileStatus is the observed state of one file the adapter
// would touch. Status is the union of "present", "missing",
// "unreadable", "would-create", "would-merge". The first three
// describe disk observation; the last two carry the adapter's
// planned action so reviewers see both "what's there" and "what
// init would do" side by side.
type DoctorFileStatus struct {
	Path     string            `json:"path"`
	Status   string            `json:"status"`
	Planned  agents.ActionKind `json:"planned_action,omitempty"`
	Keys     []string          `json:"keys,omitempty"`
	ByteSize int64             `json:"byte_size,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	// StanzaStatus is set for a present mcpServers file carrying a
	// Gortex-authored entry: "current" when it matches the canonical stanza,
	// "stale" when Gortex wrote it but the shape has since changed. Empty for
	// files that are not Gortex MCP configs. StanzaHint carries the fix.
	StanzaStatus string `json:"stanza_status,omitempty"`
	StanzaHint   string `json:"stanza_hint,omitempty"`
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	home, _ := os.UserHomeDir()
	root, err := filepath.Abs(".")
	if err != nil {
		return err
	}

	env := agents.Env{
		Root:         root,
		Home:         home,
		HookCommand:  "gortex hook",
		Mode:         agents.ModeProject,
		InstallHooks: true,
		Stderr:       nil, // suppress progress lines; doctor is read-only
	}

	if doctorRedact {
		// Install the rewriter before anything renders, so every section —
		// static, runtime, and the paths quoted inside findings — agrees on
		// what a given repo is called.
		doctorPath = newDoctorPathRedactor(home, root)
	}

	doctorEnv := doctorEnvironment()

	registry := buildRegistry()
	reports := make([]DoctorAgentReport, 0, len(registry.All()))
	for _, a := range registry.All() {
		reports = append(reports, inspectAdapter(a, env))
	}

	runtime := collectRuntime(home, doctorDays)

	if doctorJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"environment": doctorEnv,
			"agents":      reports,
			"runtime":     runtime,
		}); err != nil {
			return err
		}
		return doctorExit(runtime)
	}
	printDoctorEnvironment(cmd.OutOrStdout(), doctorEnv)
	printDoctorHuman(cmd.OutOrStdout(), reports)
	printDoctorRuntime(cmd.OutOrStdout(), runtime)
	return doctorExit(runtime)
}

// doctorExit turns a blocker into a non-zero status without printing a
// second error line — the findings already said what is wrong, and cobra
// would otherwise echo the error on top of them.
func doctorExit(r doctorRuntime) error {
	for _, a := range r.Agents {
		if doctor.HasBlocker(a.Findings) {
			return errDoctorBlocker
		}
	}
	return nil
}

// doctorEnvironment probes the live wiring: whether `gortex` resolves on PATH
// and whether the daemon answers a handshake on its socket. Both are
// best-effort and never fail the command — doctor is a read-only diagnostic.
func doctorEnvironment() DoctorEnvironment {
	out := DoctorEnvironment{DaemonSocket: daemon.SocketPath()}
	out.CLIVersion = canonicalVersion()
	if p, err := exec.LookPath("gortex"); err == nil {
		out.BinaryOnPath = true
		out.BinaryPath = p
	} else {
		out.BinaryError = err.Error()
	}
	// A real handshake (not just a socket stat) proves the daemon is alive and
	// speaking the protocol this binary expects.
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "init-doctor"})
	if err != nil {
		if errors.Is(err, daemon.ErrDaemonUnavailable) {
			out.DaemonError = "no daemon running (start it with `gortex daemon start`)"
		} else {
			out.DaemonError = err.Error()
		}
		return out
	}
	defer c.Close()
	out.DaemonRunning = true
	out.DaemonVersion = c.Ack.DaemonVersion
	// The same skew compare `gortex mcp` warns on at connect time and
	// `daemon status` renders next to the daemon version — doctor is
	// what a confused user runs first, so it must agree with both.
	out.VersionSkewWarning = daemonSkewWarning(out.DaemonVersion, out.CLIVersion)
	return out
}

// inspectMCPStanza reads a present file, looks for a Gortex-authored entry
// under mcpServers, and reports whether it matches the canonical stanza
// ("current") or has drifted ("stale"). ok is false when the file is not a
// Gortex MCP config (not JSON, no mcpServers, or the entry is user-authored),
// so non-MCP planned files are left unannotated.
func inspectMCPStanza(path string) (status string, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return "", false
	}
	servers, isMap := root["mcpServers"].(map[string]any)
	if !isMap {
		return "", false
	}
	entry, found := servers["gortex"]
	if !found {
		// Some configs key the server differently; accept any single
		// Gortex-authored entry.
		for _, v := range servers {
			if agents.IsGortexAuthoredMCPEntry(v) {
				entry, found = v, true
				break
			}
		}
	}
	if !found || !agents.IsGortexAuthoredMCPEntry(entry) {
		return "", false
	}
	if agents.MCPEntriesEqual(entry, agents.DefaultGortexMCPEntry()) {
		return "current", true
	}
	return "stale", true
}

// inspectAdapter runs Detect + Plan for one adapter, then stats
// each planned file to fill in the observed status. Never calls
// Apply, so disk is untouched.
func inspectAdapter(a agents.Adapter, env agents.Env) DoctorAgentReport {
	rep := DoctorAgentReport{Name: a.Name(), DocsURL: a.DocsURL()}
	detected, _ := a.Detect(env)
	rep.Detected = detected

	plan, err := a.Plan(env)
	if err != nil || plan == nil {
		return rep
	}

	anyPresent := false
	for _, pf := range plan.Files {
		status := DoctorFileStatus{Path: pf.Path, Planned: pf.Action, Keys: pf.Keys}
		info, err := os.Stat(pf.Path)
		switch {
		case err == nil:
			anyPresent = true
			status.Status = "present"
			status.ByteSize = info.Size()
			// For a present mcpServers config, verify the Gortex stanza is
			// the current canonical shape — Gortex owns the migration logic,
			// so doctor can flag an authored-but-outdated stanza and name the
			// one-command fix instead of silently leaving a broken wiring.
			if st, isStanza := inspectMCPStanza(pf.Path); isStanza {
				status.StanzaStatus = st
				if st == "stale" {
					status.StanzaHint = "Gortex-authored but outdated — run `gortex install` to migrate"
				}
			}
		case os.IsNotExist(err):
			status.Status = "missing"
		default:
			status.Status = "unreadable"
			status.Reason = err.Error()
		}
		rep.Files = append(rep.Files, status)
	}
	rep.Configured = anyPresent
	return rep
}

// printDoctorEnvironment renders the live-wiring preamble: the binary on PATH
// and the daemon handshake. These two lines answer "is the integration
// actually live", not just "is the config file present".
func printDoctorEnvironment(w io.Writer, env DoctorEnvironment) {
	fmt.Fprintln(w, "Gortex doctor — environment:")
	if env.BinaryOnPath {
		fmt.Fprintf(w, "  %s gortex on PATH: %s\n", glyphCheck, doctorPath(env.BinaryPath))
	} else {
		fmt.Fprintf(w, "  %s gortex not found on PATH (%s)\n", glyphCross, env.BinaryError)
	}
	if env.DaemonRunning {
		ver := env.DaemonVersion
		if ver == "" {
			ver = "ok"
		}
		// A skewed pair downgrades the handshake row to a warning and
		// appends the same one-line remedy the proxy prints — doctor,
		// `daemon status`, and the MCP proxy must give one verdict.
		glyph := glyphCheck
		if env.VersionSkewWarning != "" {
			glyph = glyphWarn
		}
		fmt.Fprintf(w, "  %s daemon handshake: %s (%s)\n", glyph, ver, doctorPath(env.DaemonSocket))
		if env.VersionSkewWarning != "" {
			fmt.Fprintf(w, "    %s\n", env.VersionSkewWarning)
		}
	} else {
		fmt.Fprintf(w, "  %s daemon handshake: %s\n", glyphCross, env.DaemonError)
	}
	fmt.Fprintln(w)
}

// The doctor's status markers. The distinction between glyphAbsent and
// glyphCross is the whole point of having both: a per-repo file that `gortex
// init` has not written is not a fault — a machine-wide `gortex install`
// covers the agent, and most repos never need repo-local config at all.
// Marking it ✗ turned an optional file into an accusation and implied work
// the user does not have to do. ✗ is reserved for a real gap: something
// broken, outdated, or genuinely not wired up.
const (
	glyphCheck  = "✓"
	glyphCross  = "✗"
	glyphWarn   = "!"
	glyphAbsent = "·"
)

// printDoctorHuman renders the human-readable summary. One row per
// agent, with a nested file list. Columns line up for copy-paste
// into issue reports.
// printDoctorHuman renders the adapter section. Adapters that are neither
// installed nor carrying Gortex files collapse to a name list: their planned
// paths describe writes that will never happen, and one uninstalled agent can
// contribute twenty of them — burying the handful of lines that describe the
// machine the user actually has. An absent agent that still holds Gortex files
// is kept and called out instead: that is leftover config, which is a finding.
// --all restores the full listing, and --json is always complete.
func printDoctorHuman(w io.Writer, reports []DoctorAgentReport) {
	fmt.Fprintln(w, "Gortex doctor — observed state of every detected adapter:")
	fmt.Fprintf(w, "  %s present   %s absent (optional — repo-local config)   %s needs attention\n",
		glyphCheck, glyphAbsent, glyphWarn)
	fmt.Fprintln(w)

	var absent []string
	for _, r := range reports {
		if !doctorAll && !r.Detected && !r.Configured {
			absent = append(absent, r.Name)
			continue
		}
		detMark := "–"
		if r.Detected {
			detMark = "✓"
		}
		cfgMark := "–"
		if r.Configured {
			cfgMark = "✓"
		}
		fmt.Fprintf(w, "  [%s installed] [%s gortex files]  %s\n", detMark, cfgMark, r.Name)
		for _, f := range r.Files {
			statusSym := "?"
			switch f.Status {
			case "present":
				statusSym = glyphCheck
			case "missing":
				// Absent, not wrong: repo-local config is optional on a
				// machine configured by `gortex install`.
				statusSym = glyphAbsent
			case "unreadable":
				statusSym = glyphWarn
			}
			extra := ""
			if f.Status == "present" && f.ByteSize > 0 {
				extra = fmt.Sprintf(" (%d bytes)", f.ByteSize)
			}
			switch f.StanzaStatus {
			case "stale":
				// Present but outdated is a real gap — Gortex wrote this and
				// the shape has since moved on.
				statusSym = glyphWarn
				extra += " [outdated stanza: `gortex upgrade --run` migrates it, or `gortex install`]"
			case "current":
				extra += " [stanza current]"
			}
			if f.Status == "missing" && f.Planned != "" {
				// Strip the "would-" prefix so we don't print
				// "init would would-create".
				verb := string(f.Planned)
				if len(verb) > len("would-") && verb[:len("would-")] == "would-" {
					verb = verb[len("would-"):]
				}
				extra = fmt.Sprintf(" (optional — `gortex init` would %s)", verb)
			}
			fmt.Fprintf(w, "      %s %s%s\n", statusSym, doctorPath(f.Path), extra)
		}
		if !r.Detected && r.Configured {
			fmt.Fprintln(w, "      note: not installed, but Gortex files are present — leftover config.")
		}
		if r.DocsURL != "" {
			fmt.Fprintf(w, "      docs: %s\n", r.DocsURL)
		}
		fmt.Fprintln(w)
	}

	if len(absent) > 0 {
		fmt.Fprintf(w, "  not installed (%d): %s\n", len(absent), strings.Join(absent, ", "))
		fmt.Fprintln(w, "  nothing is written for these; --all lists what init would write if they were.")
		fmt.Fprintln(w)
	}
}
