package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/docs"
)

// stringArgOrDefault returns args[key] coerced to a trimmed string,
// or fallback when the key is absent or empty.
func stringArgOrDefault(args map[string]any, key, fallback string) string {
	if v := stringArg(args, key); v != "" {
		return v
	}
	return fallback
}

// intArgOrDefault returns args[key] coerced to an int, or fallback
// when the key is absent/zero. Both float64 and int forms are
// accepted (JSON-unmarshal vs. Go-callsite).
func intArgOrDefault(args map[string]any, key string, fallback int) int {
	switch v := args[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	}
	return fallback
}

// boolArgValue returns args[key] as a bool, false when absent.
// Thin wrapper around boolArg that drops the "ok" return for callers
// that want zero-value semantics.
func boolArgValue(args map[string]any, key string) bool {
	v, _ := boolArg(args, key)
	return v
}

// parseDurationArg accepts a duration argument expressed as a Go
// duration string (e.g. "24h", "30m", "7d"). The "Nd" suffix is a
// gortex extension — Go's stdlib doesn't accept days natively, so we
// translate it to hours here.
func parseDurationArg(args map[string]any, key string) (time.Duration, error) {
	raw := strings.TrimSpace(stringArg(args, key))
	if raw == "" {
		return 0, nil
	}
	if strings.HasSuffix(raw, "d") {
		core := strings.TrimSuffix(raw, "d")
		if d, err := time.ParseDuration(core + "h"); err == nil {
			return d * 24, nil
		}
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	return d, nil
}

// writeWikiFile is a thin wrapper around os.WriteFile that ensures
// the parent directory exists. Used by the MCP handler when the
// caller passes an output_path.
func writeWikiFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, body, 0o644)
}

// docsHistoryProvider adapts the server's watcher into the
// docs-package shape. Returns nil when no watcher is attached, in
// which case the recent-changes section will be empty.
func (s *Server) docsHistoryProvider() docs.HistoryProvider {
	watcher := s.currentWatcher()
	if watcher == nil {
		return nil
	}
	return &watcherHistoryAdapter{w: watcher}
}

type watcherHistoryAdapter struct {
	w watcherHistory
}

func (a *watcherHistoryAdapter) HistorySince(since time.Time) []docs.HistoryEvent {
	if a.w == nil {
		return nil
	}
	src := a.w.HistorySince(since)
	out := make([]docs.HistoryEvent, 0, len(src))
	for _, ev := range src {
		out = append(out, docs.HistoryEvent{
			FilePath:     ev.FilePath,
			Kind:         string(ev.Kind),
			NodesAdded:   ev.NodesAdded,
			NodesRemoved: ev.NodesRemoved,
			Timestamp:    ev.Timestamp,
		})
	}
	return out
}

// guardGeneratedOutputPath confines a caller-named output path for the
// generator tools (generate_wiki, generate_docs) to the indexed repository
// roots, on the same terms export_graph already applies to its own output
// arguments.
//
// Those two tools wrote wherever the argument pointed: generate_docs took
// output_path straight to os.MkdirAll + os.WriteFile, and generate_wiki's
// output_dir became the writer root. A prompt-injected agent could therefore
// drive the daemon into creating a tree — or truncating a file — anywhere the
// daemon user can write, which is exactly what the export path was hardened
// against. The guard is skipped for the operator-driven CLI / control channel
// (confineCallerPaths), which asks for the write in the user's own name.
func (s *Server) guardGeneratedOutputPath(ctx context.Context, path string) error {
	if path == "" || !s.confineCallerPaths(ctx) {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve output path %q: %w", path, err)
	}
	return s.guardSymlinkWithinRepo(ctx, abs)
}
