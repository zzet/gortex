package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents/claudecode"
	semver "github.com/zzet/gortex/internal/version"
)

var (
	pluginEmitTarget  string
	pluginEmitVariant string
	pluginEmitVersion string
)

var pluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "Manage Gortex marketplace plugin bundles",
	Long: `Generate plugin bundles for agent-host marketplaces.

Today this command emits the Anthropic Plugin Marketplace layout —
a directory containing plugin.json, README, LICENSE, .mcp.json,
slash commands, skills, and hooks. The Cursor variant lands in a
follow-up.

The marketplace bundle is generated, not hand-maintained. The single
source of truth lives in internal/agents/commands_bodies.go and
this command serialises that content into the directory layout the
marketplace expects.`,
}

var pluginEmitCmd = &cobra.Command{
	Use:   "emit",
	Short: "Emit a plugin bundle into a target directory",
	Long: `Emit the marketplace plugin bundle.

Example:
  gortex plugin emit --target ./claude-plugin --variant anthropic --version 0.18.2

The target directory is created if missing; existing files are
overwritten so re-runs converge on a deterministic output.`,
	RunE: runPluginEmit,
}

func init() {
	pluginEmitCmd.Flags().StringVar(&pluginEmitTarget, "target", "", "target directory to emit the bundle into (required)")
	pluginEmitCmd.Flags().StringVar(&pluginEmitVariant, "variant", string(claudecode.LayoutVariantAnthropic), "marketplace layout variant: anthropic")
	pluginEmitCmd.Flags().StringVar(&pluginEmitVersion, "version", "", "gortex SemVer to bake into plugin.json (defaults to the running binary's version)")

	pluginCmd.AddCommand(pluginEmitCmd)
	rootCmd.AddCommand(pluginCmd)
}

func runPluginEmit(_ *cobra.Command, _ []string) error {
	if pluginEmitTarget == "" {
		return fmt.Errorf("--target is required")
	}
	v := pluginEmitVersion
	if v == "" {
		v = cleanSemver(version)
	}
	abs, err := filepath.Abs(pluginEmitTarget)
	if err != nil {
		return fmt.Errorf("resolve target dir: %w", err)
	}
	written, err := claudecode.EmitPluginBundle(claudecode.PluginBundleSpec{
		TargetDir:     abs,
		Version:       v,
		LayoutVariant: claudecode.LayoutVariant(pluginEmitVariant),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(rootCmd.OutOrStdout(), "Wrote %d files to %s (variant=%s, version=%s)\n", len(written), abs, pluginEmitVariant, v)
	return nil
}

// cleanSemver normalises a build-time-injected version string into the
// `MAJOR.MINOR.PATCH` form the marketplace plugin.json expects. The
// running binary's `main.version` may be either a clean semver from
// main.go (e.g. "0.18.2") or git-describe output with build slots
// (e.g. "v0.18.2-1-ga4997e7-dirty") depending on whether ldflags were
// injected. We strip the leading 'v', drop any prerelease/build slot,
// and drop a trailing `-N-g<sha>[-dirty]` git-describe suffix.
//
// On parse failure we return the raw input; the marketplace publish
// step rejects bad versions in its own schema validation, so the
// upstream PR review catches anything we miss here.
func cleanSemver(s string) string {
	if s == "" {
		return s
	}
	// First try a structural parse so canonical "v0.18.2" or
	// "0.18.2-rc.1" round-trip cleanly.
	if v, err := semver.Parse(s); err == nil {
		return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	// git-describe output looks like "v0.18.2-1-ga4997e7" or
	// "v0.18.2-1-ga4997e7-dirty". Strip everything from the first
	// "-N-g<sha>" boundary onward and try again.
	stripped := strings.TrimPrefix(s, "v")
	if idx := strings.Index(stripped, "-"); idx > 0 {
		// Anything after the first dash that doesn't parse as a
		// SemVer prerelease is probably a git-describe trailer.
		head := stripped[:idx]
		if _, err := semver.Parse(head); err == nil {
			return head
		}
	}
	return s
}
