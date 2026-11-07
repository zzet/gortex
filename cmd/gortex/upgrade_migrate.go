package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/opencode"
)

// upgrade_migrate.go re-applies agent configuration after a successful binary
// upgrade.
//
// Config shapes drift between releases — an MCP stanza gains a field, a hook
// command changes, an instructions block is rewritten. Until now the only
// thing that noticed was `gortex doctor`, which told the user to go run
// `gortex install`. That reads as a chore handed back: upgrading is exactly
// when the configs should be brought current, and the user already asked for
// the new version by running this command.
//
// Two constraints shape the implementation.
//
// First, the migration must run from the NEW binary. This process is the old
// one — the package manager replaced the file underneath it — so re-applying
// adapters in-process would faithfully write the shapes we are trying to
// migrate away from. We re-exec instead.
//
// Second, re-applying can rewrite a hook definition, and Codex records trust
// against each hook's hash: a changed hook is skipped until the user
// re-approves it in /hooks. Silently re-trusting is not an option, so the
// migration deliberately runs through `gortex install`, whose summary already
// carries that notice, rather than a bespoke quieter migrator.

// upgradeNoMigrate opts out of the post-upgrade config refresh.
var upgradeNoMigrate bool

// upgradeMigrateCommand builds the re-exec. A package var so tests can assert
// on the invocation without running an installer.
var upgradeMigrateCommand = func(ctx context.Context, bin string) *exec.Cmd {
	// --yes keeps it non-interactive: the user already consented by upgrading,
	// and a wizard prompt inside an upgrade would be a surprise.
	args := []string{"install", "--yes"}
	// install defaults to --hook-mode=deny, so without this a machine set
	// to enrich would be reset by every upgrade. The installed hooks are the
	// only record of the posture; read it back here, before the re-exec.
	if home, err := os.UserHomeDir(); err == nil {
		if mode, ok := installedHookMode(home); ok {
			args = append(args, "--hook-mode="+mode)
		}
	}
	return exec.CommandContext(ctx, bin, args...)
}

// installedHookMode is the one posture the installed hooks agree on. Claude
// Code's settings and the OpenCode bridge each carry one; when both exist
// and differ, no single --hook-mode would keep both, so none is passed and
// install's default applies, as it always did.
func installedHookMode(home string) (string, bool) {
	claude, haveClaude := claudecode.InstalledHookMode(home)
	bridge, haveBridge := opencode.InstalledHookMode(home)
	switch {
	case haveClaude && haveBridge && claude != bridge:
		return "", false
	case haveClaude:
		return claude, true
	default:
		return bridge, haveBridge
	}
}

// resolveUpgradedBinary finds the gortex that the upgrade just installed.
// PATH first: `go install` can land the new binary somewhere other than where
// the old one ran from, and re-execing the stale path would migrate using the
// very code we replaced. os.Executable is the fallback for the common case
// where the package manager overwrote the file in place.
func resolveUpgradedBinary() (string, error) {
	if p, err := exec.LookPath("gortex"); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			return abs, nil
		}
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the upgraded gortex binary: %w", err)
	}
	return exe, nil
}

// migrateAgentConfigs re-applies every detected adapter's user-level config
// with the freshly installed binary. Best-effort by design: the upgrade
// itself has already succeeded, so a migration that cannot run is a warning
// with a manual command, never a failed upgrade.
func migrateAgentConfigs(ctx context.Context, out, errw io.Writer) {
	bin, err := resolveUpgradedBinary()
	if err != nil {
		fmt.Fprintf(errw, "warning: %v — run `gortex install` to refresh agent config.\n", err)
		return
	}

	fmt.Fprintln(out, "\nRefreshing agent configuration with the new binary:")
	cmd := upgradeMigrateCommand(ctx, bin)
	cmd.Stdout, cmd.Stderr = out, errw
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(errw, "warning: agent config refresh failed (%v) — run `gortex install` by hand.\n", err)
	}
}

// upgradeFollowUps renders what still has to happen after the binary lands.
// The reindex advisory is unconditional (a new binary may carry newer
// extractors, so an indexed graph can be stale). The config step is listed
// only when we did not just perform it.
func upgradeFollowUps(out io.Writer, ran, migrated bool) {
	var steps []string
	if !migrated {
		// Either a dry run (we only printed the upgrade command) or --no-migrate.
		steps = append(steps, "gortex install    # refresh agent config for the new version")
	}
	steps = append(steps, "gortex index .    # reindex so the graph picks up extractor changes")

	lead := "After upgrading:"
	if ran {
		lead = "Next:"
	}
	fmt.Fprintf(out, "\n%s\n", lead)
	for _, s := range steps {
		fmt.Fprintf(out, "  %s\n", s)
	}
}

// upgradeMigrationSkipped explains a --no-migrate run so the omission is
// visible rather than silent.
func upgradeMigrationSkipped(out io.Writer) {
	fmt.Fprintln(out, "\nSkipping the agent config refresh (--no-migrate).")
}

// postUpgradeSteps runs what has to happen once the new binary is on disk and
// reports whether the config refresh was performed. Split out from runUpgrade
// so the decision is testable without a real install method, a real release
// check, or a real package manager.
func postUpgradeSteps(ctx context.Context, out, errw io.Writer, noMigrate bool) bool {
	if noMigrate {
		upgradeMigrationSkipped(out)
		return false
	}
	migrateAgentConfigs(ctx, out, errw)
	return true
}
