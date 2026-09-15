package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/githooks"
)

var (
	githookRegenMermaid   bool
	githookRegenWiki      bool
	githookRegenDocs      bool
	githookRegenChurn     bool
	githookRegenReleases  bool
	githookMermaidOutDir  string
	githookWikiOutDir     string
	githookDocsOutPath    string
	githookChurnBranch    string
	githookReleasesBranch string
	githookBinary         string
	githookHookTimeout    int
)

var githookCmd = &cobra.Command{
	Use:   "githook",
	Short: "Manage local git hooks that regenerate gortex artefacts",
	Long: `Install, uninstall, and inspect git hooks that re-run gortex
commands. Supported hooks: post-commit, post-merge, post-checkout.

The hook is idempotent: re-running install replaces only the gortex
block, leaving any other hook content intact. Uninstall removes the
block and deletes the hook file when it contains nothing else.

Every gortex invocation the hook makes is bounded by a watchdog
(default 30s per command; --hook-timeout to change; 0 restores the old
unbounded lines) when GNU timeout or perl is available, so a busy
daemon cannot hang the git operation indefinitely.`,
}

var githookInstallCmd = &cobra.Command{
	Use:   "install <hook>",
	Short: "Install a git hook (post-commit, post-merge, or post-checkout)",
	Args:  cobra.ExactArgs(1),
	RunE:  runGithookInstall,
}

var githookUninstallCmd = &cobra.Command{
	Use:   "uninstall <hook>",
	Short: "Remove the gortex-managed block from a git hook",
	Args:  cobra.ExactArgs(1),
	RunE:  runGithookUninstall,
}

var githookStatusCmd = &cobra.Command{
	Use:   "status [hook]",
	Short: "Report whether the named hook is gortex-managed (default: post-commit)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runGithookStatus,
}

func init() {
	githookInstallCmd.Flags().BoolVar(&githookRegenMermaid, "regen-mermaid", false,
		"include `gortex export --format mermaid --scope all` in the hook")
	githookInstallCmd.Flags().BoolVar(&githookRegenWiki, "regen-wiki", false,
		"include `gortex wiki .` in the hook")
	githookInstallCmd.Flags().BoolVar(&githookRegenDocs, "regen-docs", false,
		"include `gortex docs . --out CHANGELOG_AUTO.md` in the hook")
	githookInstallCmd.Flags().BoolVar(&githookRegenChurn, "regen-churn", false,
		"include `gortex enrich churn` so get_churn_rate stays fresh without an at-read-time git subprocess")
	githookInstallCmd.Flags().StringVar(&githookChurnBranch, "churn-branch", "",
		"branch / tag / SHA the churn enricher pins to (default: resolve at hook run-time)")
	githookInstallCmd.Flags().BoolVar(&githookRegenReleases, "regen-releases", false,
		"include `gortex enrich releases` so analyze kind=releases reads pre-computed Meta")
	githookInstallCmd.Flags().StringVar(&githookReleasesBranch, "releases-branch", "",
		"branch / tag / SHA the releases enricher restricts to (default: resolve at hook run-time)")
	githookInstallCmd.Flags().StringVar(&githookMermaidOutDir, "mermaid-out-dir", "docs/architecture/",
		"output directory for mermaid diagrams")
	githookInstallCmd.Flags().StringVar(&githookWikiOutDir, "wiki-out-dir", "wiki",
		"output directory for the wiki")
	githookInstallCmd.Flags().StringVar(&githookDocsOutPath, "docs-out-path", "CHANGELOG_AUTO.md",
		"output path for the docs bundle")
	githookInstallCmd.Flags().StringVar(&githookBinary, "binary", "gortex",
		"gortex binary name (resolved from $PATH at runtime)")
	githookInstallCmd.Flags().IntVar(&githookHookTimeout, "hook-timeout", 30,
		"seconds a hook-invoked gortex command may run before the watchdog kills it (0 = unbounded)")

	githookCmd.AddCommand(githookInstallCmd)
	githookCmd.AddCommand(githookUninstallCmd)
	githookCmd.AddCommand(githookStatusCmd)
	rootCmd.AddCommand(githookCmd)
}

// supportedHook validates the hook arg. We mirror the package-level
// SupportedHooks list rather than importing it so the CLI surface
// stays decoupled from the install package's internals.
func supportedHook(name string) error {
	if name == "post-commit" || name == "post-merge" || name == "post-checkout" {
		return nil
	}
	return fmt.Errorf("unsupported hook %q (supported: post-commit, post-merge, post-checkout)", name)
}

func runGithookInstall(cmd *cobra.Command, args []string) error {
	hook := args[0]
	if err := supportedHook(hook); err != nil {
		return err
	}
	if githookHookTimeout < 0 {
		return fmt.Errorf("--hook-timeout must be >= 0 (got %d; 0 = unbounded)", githookHookTimeout)
	}
	repoRoot, err := resolveGithookRepoRoot()
	if err != nil {
		return err
	}
	if !githookRegenMermaid && !githookRegenWiki && !githookRegenDocs && !githookRegenChurn && !githookRegenReleases {
		// Default to mermaid when nothing was chosen — minimum
		// useful behaviour.
		githookRegenMermaid = true
	}
	path, err := githooks.InstallHook(repoRoot, hook, githooks.InstallOpts{
		Binary:             githookBinary,
		RegenMermaid:       githookRegenMermaid,
		RegenWiki:          githookRegenWiki,
		RegenDocs:          githookRegenDocs,
		RegenChurn:         githookRegenChurn,
		ChurnBranch:        githookChurnBranch,
		RegenReleases:      githookRegenReleases,
		ReleasesBranch:     githookReleasesBranch,
		MermaidOutDir:      githookMermaidOutDir,
		WikiOutDir:         githookWikiOutDir,
		DocsOutPath:        githookDocsOutPath,
		HookTimeoutSeconds: githookHookTimeout,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"installed %s hook at %s\nactions: mermaid=%t wiki=%t docs=%t churn=%t releases=%t timeout=%ds\n",
		hook, path, githookRegenMermaid, githookRegenWiki, githookRegenDocs, githookRegenChurn, githookRegenReleases, githookHookTimeout)
	return nil
}

func runGithookUninstall(cmd *cobra.Command, args []string) error {
	hook := args[0]
	if err := supportedHook(hook); err != nil {
		return err
	}
	repoRoot, err := resolveGithookRepoRoot()
	if err != nil {
		return err
	}
	path, removed, err := githooks.UninstallHook(repoRoot, hook)
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintf(cmd.OutOrStdout(), "removed gortex block from %s\n", path)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "no gortex block found at %s\n", path)
	}
	return nil
}

func runGithookStatus(cmd *cobra.Command, args []string) error {
	hook := "post-commit"
	if len(args) > 0 {
		if err := supportedHook(args[0]); err != nil {
			return err
		}
		hook = args[0]
	}
	repoRoot, err := resolveGithookRepoRoot()
	if err != nil {
		return err
	}
	hookPath, err := githooks.HookPathFor(repoRoot, hook)
	if err != nil {
		return err
	}
	// Read directly; Status() is post-commit-locked and we want per-hook
	// detail. Mirrors Status() but parameterised on hook.
	body, ferr := os.ReadFile(hookPath)
	exists := ferr == nil
	managed := false
	if exists {
		bs := string(body)
		begin := "# gortex-managed:" + hook + ":begin"
		end := "# gortex-managed:" + hook + ":end"
		if strings.Contains(bs, begin) && strings.Contains(bs, end) {
			managed = true
		}
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "hook:      %s\n", hook)
	fmt.Fprintf(out, "hook_path: %s\n", hookPath)
	fmt.Fprintf(out, "exists:    %t\n", exists)
	fmt.Fprintf(out, "managed:   %t\n", managed)
	return nil
}

// resolveGithookRepoRoot picks the repo root for the active command.
// Today we always use the working directory; the install runs git
// itself to resolve git-dir and core.hooksPath.
func resolveGithookRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Clean(cwd), nil
}
