package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
)

var (
	queryIndex  string
	queryDepth  int
	queryFormat string
	queryLimit  int
)

var queryCmd = &cobra.Command{
	Use:   "query",
	Short: "Query the knowledge graph",
}

func init() {
	queryCmd.PersistentFlags().StringVar(&queryIndex, "index", ".", "repository path the daemon must track")
	queryCmd.PersistentFlags().IntVar(&queryDepth, "depth", 3, "traversal depth")
	queryCmd.PersistentFlags().StringVar(&queryFormat, "format", "text", "output format: text|json (traversal queries also support dot|mermaid)")
	queryCmd.PersistentFlags().IntVar(&queryLimit, "limit", 50, "max nodes in result")

	queryCmd.AddCommand(querySymbolCmd)
	queryCmd.AddCommand(queryDepsCmd)
	queryCmd.AddCommand(queryDependentsCmd)
	queryCmd.AddCommand(queryCallersCmd)
	queryCmd.AddCommand(queryCallsCmd)
	queryCmd.AddCommand(queryImplementationsCmd)
	queryCmd.AddCommand(queryUsagesCmd)
	queryCmd.AddCommand(queryStatsCmd)

	rootCmd.AddCommand(queryCmd)
}

// requireDaemonTool runs a graph tool against the daemon that owns the
// repo. The daemon is the single graph owner: when none is running, or it
// does not track the repo, this returns an actionable error rather than
// silently building a second-class in-process index.
func requireDaemonTool(repoPath, tool string, args map[string]any) (json.RawMessage, error) {
	return requireDaemonToolWithSurface(repoPath, tool, args, cliLegacyToolSurface, cliLegacyToolMode)
}

// requireDaemonToolWithSurface runs one tool through a per-connection MCP
// surface without changing daemon-global configuration. It shares the normal
// daemon-required and repo-not-tracked error behavior.
func requireDaemonToolWithSurface(repoPath, tool string, args map[string]any, tools, toolsMode string) (json.RawMessage, error) {
	exec, err := resolveExecutorWithToolSurface(repoPath, tools, toolsMode)
	if err != nil {
		if errors.Is(err, ErrNoExecutor) {
			return nil, daemonRequiredErr(repoPath)
		}
		return nil, err
	}
	defer exec.Close()
	out, err := exec.CallTool(context.Background(), tool, args)
	if err != nil {
		if errors.Is(err, ErrRepoNotTracked) {
			return nil, daemonRequiredErr(repoPath)
		}
		return nil, err
	}
	return out, nil
}

// daemonRequiredErr explains how to make the daemon able to answer: start
// it (when none runs) or track the repo (when it runs but does not own it).
//
// A linked git worktree takes neither remedy verbatim — the repository to
// track is its main checkout — so it is answered separately. That resolution
// is a filesystem fact, which is what makes it available on the arm where the
// daemon is down and nothing can be asked.
func daemonRequiredErr(repoPath string) error {
	abs, aerr := filepath.Abs(repoPath)
	if aerr != nil {
		abs = repoPath
	}
	if fam, ok := linkedWorktreeAt(abs); ok {
		return worktreeCWDErr(abs, fam, trackedFamilyRepo(fam))
	}
	if !daemon.IsRunning() {
		return fmt.Errorf("no gortex daemon is running — start it with `gortex daemon start --detach`, then track this repo with `gortex track %s`", abs)
	}
	return fmt.Errorf("the gortex daemon does not track %s — add it with `gortex track %s`", abs, abs)
}

// emitDaemonJSON re-indents and prints a daemon tool result for the
// --format json path (and as the fallback for unrecognised shapes).
func emitDaemonJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return nil
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printDaemonSearchSymbols(cmd *cobra.Command, raw json.RawMessage) error {
	if queryFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload struct {
		Results []struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			FilePath string `json:"file_path"`
			Line     int    `json:"start_line"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	for _, r := range payload.Results {
		fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-40s %s:%d\n", r.Kind, r.ID, r.FilePath, r.Line)
	}
	return nil
}

// printDaemonSubgraph renders the node-list shape shared by the graph
// traversal tools (get_dependencies / dependents / callers / call_chain /
// find_usages / find_implementations). Falls back to pretty JSON when the
// payload is not the expected shape.
func printDaemonSubgraph(cmd *cobra.Command, raw json.RawMessage) error {
	if queryFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload struct {
		Nodes []struct {
			ID        string `json:"id"`
			Kind      string `json:"kind"`
			FilePath  string `json:"file_path"`
			Line      int    `json:"line"`
			StartLine int    `json:"start_line"`
		} `json:"nodes"`
		Truncated  bool `json:"truncated"`
		TotalNodes int  `json:"total_nodes"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Nodes == nil {
		return emitDaemonJSON(cmd, raw)
	}
	for _, n := range payload.Nodes {
		line := n.Line
		if line == 0 {
			line = n.StartLine
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-40s %s:%d\n", n.Kind, n.ID, n.FilePath, line)
	}
	if payload.Truncated {
		fmt.Fprintf(cmd.OutOrStdout(), "... truncated (%d total)\n", payload.TotalNodes)
	}
	return nil
}

// emitSubgraph runs a graph traversal tool and renders it per --format.
// text/json go through the node-list renderer; dot/mermaid ask the daemon
// for the diagram directly (its query.SubGraph already knows how to draw
// itself) and print it verbatim.
func emitSubgraph(cmd *cobra.Command, repoPath, tool string, args map[string]any) error {
	diagram := queryFormat == "dot" || queryFormat == "mermaid"
	if diagram {
		args["format"] = queryFormat
	}
	out, err := requireDaemonTool(repoPath, tool, args)
	if err != nil {
		return err
	}
	if diagram {
		fmt.Fprintln(cmd.OutOrStdout(), string(out))
		return nil
	}
	return printDaemonSubgraph(cmd, out)
}

func printDaemonStats(cmd *cobra.Command, raw json.RawMessage) error {
	if queryFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload struct {
		TotalNodes int            `json:"total_nodes"`
		TotalEdges int            `json:"total_edges"`
		ByKind     map[string]int `json:"by_kind"`
		ByLanguage map[string]int `json:"by_language"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return emitDaemonJSON(cmd, raw)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Nodes: %d  Edges: %d\n", payload.TotalNodes, payload.TotalEdges)
	if len(payload.ByKind) > 0 {
		fmt.Fprintln(out, "By kind:")
		for k, v := range payload.ByKind {
			fmt.Fprintf(out, "  %-12s %d\n", k, v)
		}
	}
	if len(payload.ByLanguage) > 0 {
		fmt.Fprintln(out, "By language:")
		for k, v := range payload.ByLanguage {
			fmt.Fprintf(out, "  %-12s %d\n", k, v)
		}
	}
	return nil
}

var querySymbolCmd = &cobra.Command{
	Use:   "symbol <name>",
	Short: "Find symbols matching name",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out, err := requireDaemonTool(queryIndex, "search_symbols",
			map[string]any{"query": args[0], "limit": queryLimit})
		if err != nil {
			return err
		}
		return printDaemonSearchSymbols(cmd, out)
	},
}

var queryDepsCmd = &cobra.Command{
	Use:   "deps <id>",
	Short: "Show dependencies of a symbol",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return emitSubgraph(cmd, queryIndex, "get_dependencies",
			map[string]any{"id": args[0], "depth": queryDepth, "limit": queryLimit})
	},
}

var queryDependentsCmd = &cobra.Command{
	Use:   "dependents <id>",
	Short: "Show blast radius for a symbol",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return emitSubgraph(cmd, queryIndex, "get_dependents",
			map[string]any{"id": args[0], "depth": queryDepth, "limit": queryLimit})
	},
}

var queryCallersCmd = &cobra.Command{
	Use:   "callers <func-id>",
	Short: "Show who calls a function",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return emitSubgraph(cmd, queryIndex, "get_callers",
			map[string]any{"id": args[0], "depth": queryDepth, "limit": queryLimit})
	},
}

var queryCallsCmd = &cobra.Command{
	Use:   "calls <func-id>",
	Short: "Show what a function calls",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return emitSubgraph(cmd, queryIndex, "get_call_chain",
			map[string]any{"id": args[0], "depth": queryDepth, "limit": queryLimit})
	},
}

var queryImplementationsCmd = &cobra.Command{
	Use:   "implementations <interface-id>",
	Short: "Show implementations of an interface",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return emitSubgraph(cmd, queryIndex, "find_implementations",
			map[string]any{"id": args[0]})
	},
}

var queryUsagesCmd = &cobra.Command{
	Use:   "usages <id>",
	Short: "Show all usages of a symbol",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return emitSubgraph(cmd, queryIndex, "find_usages",
			map[string]any{"id": args[0], "limit": queryLimit})
	},
}

var queryStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show graph statistics",
	RunE: func(cmd *cobra.Command, _ []string) error {
		out, err := requireDaemonTool(queryIndex, "graph_stats", map[string]any{})
		if err != nil {
			return err
		}
		return printDaemonStats(cmd, out)
	},
}
