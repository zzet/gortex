package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/eval"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/server"
)

var (
	evalPort  int
	evalIndex string
)

var evalServerCmd = &cobra.Command{
	Use:   "eval-server",
	Short: "Start the eval HTTP server for benchmarking",
	Long:  "Starts an HTTP daemon wrapping Gortex MCP tools for evaluation. Exposes /health, /tool/{name}, /augment, and /stats endpoints.",
	RunE:  runEvalServer,
}

var (
	evalBind      string
	evalAuthToken string
)

func init() {
	evalServerCmd.Flags().IntVar(&evalPort, "port", 4747, "HTTP port to listen on")
	evalServerCmd.Flags().StringVar(&evalBind, "bind", "127.0.0.1", "bind address; a non-loopback bind requires --auth-token")
	evalServerCmd.Flags().StringVar(&evalAuthToken, "auth-token", "", "bearer token required for every request (fallback: $GORTEX_EVAL_TOKEN)")
	evalServerCmd.Flags().StringVar(&evalIndex, "index", "", "repository path to index on startup")
	rootCmd.AddCommand(evalServerCmd)
}

func runEvalServer(cmd *cobra.Command, args []string) error {
	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Build the same graph/parser/indexer/query/MCP stack as serve.go.
	g, closeStore, err := newEvalStore("server")
	if err != nil {
		return fmt.Errorf("opening eval store: %w", err)
	}
	defer closeStore()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, cfg.Index, logger)

	eng := query.NewEngine(g)
	eng.SetSearch(idx.Search())
	gortexmcp.Version = version
	srv := gortexmcp.NewServer(eng, g, idx, nil, logger, cfg.Guards.Rules)
	defer srv.DrainBackground()
	srv.SetArchitecture(cfg.Architecture)
	srv.SetEventRules(cfg.Events.Rules)
	srv.SetArtifacts(cfg.Artifacts)
	srv.SetNamedQueries(cfg.Queries)

	// Index the repository if --index is provided.
	if evalIndex != "" {
		fmt.Fprintf(os.Stderr, "[gortex] eval-server: indexing %s...\n", evalIndex)
		result, err := idx.Index(evalIndex)
		if err != nil {
			return fmt.Errorf("indexing %s: %w", evalIndex, err)
		}
		fmt.Fprintf(os.Stderr, "[gortex] eval-server: indexed %d files (%d nodes, %d edges) in %dms\n",
			result.FileCount, result.NodeCount, result.EdgeCount, result.DurationMs)
	}

	// Run analysis (communities, processes) after indexing.
	srv.RunAnalysis()

	// Wire the MCP server's tool dispatch into an HTTP handler.
	handler := eval.NewHandler(srv.MCPServer(), g, version, logger)

	// Bind loopback by default and refuse a wider bind without a token.
	// This surface publishes the daemon's whole tool catalogue — including
	// file reads and writes — so binding every interface unauthenticated
	// handed the machine's indexed repositories to anyone who could route
	// to the port.
	authToken := evalAuthToken
	if authToken == "" {
		authToken = os.Getenv("GORTEX_EVAL_TOKEN")
	}
	addr := net.JoinHostPort(evalBind, strconv.Itoa(evalPort))
	if err := httpTokenRequirementError(addr, authToken); err != nil {
		return err
	}
	if authToken == "" {
		fmt.Fprintln(os.Stderr, "[gortex] eval-server: unauthenticated mode; localhost only")
	}
	httpServer := &http.Server{
		Addr:    addr,
		Handler: server.WithAuth(handler, authToken),
	}

	fmt.Fprintf(os.Stderr, "[gortex] eval-server listening on http://%s\n", addr)

	// Start HTTP server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, platform.ShutdownSignals()...)

	select {
	case err := <-errCh:
		return fmt.Errorf("eval-server: %w", err)
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\n[gortex] eval-server: received %s, shutting down\n", sig)
		return httpServer.Close()
	}
}
