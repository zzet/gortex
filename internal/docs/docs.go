// Package docs generates a "living changelog + ownership + blame +
// stale code" bundle from the graph store. Output is markdown or
// JSON; the CLI verb `gortex docs` and the MCP tool `generate_docs`
// both call into here.
//
// The package depends on:
//   - graph.Graph (for symbol nodes and blame metadata).
//   - optional indexer.HistoryProvider (for recent change events).
//   - blame.EnrichGraph (callable via the BlameRunner injection).
//
// All four sections are independent — passing Sections lets the caller
// pick a subset (e.g. only recent changes for a post-commit hook).
package docs

import (
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// HistoryEvent is the docs-package projection of one watcher event.
// Kept local so the package doesn't import internal/indexer.
type HistoryEvent struct {
	FilePath     string    `json:"file_path"`
	Kind         string    `json:"kind"`
	NodesAdded   int       `json:"nodes_added"`
	NodesRemoved int       `json:"nodes_removed"`
	Timestamp    time.Time `json:"timestamp"`
}

// HistoryProvider is the minimal surface docs needs from the indexer.
// Both indexer.Watcher and indexer.MultiWatcher already implement
// HistorySince natively; the MCP / CLI callers adapt them through a
// thin shim.
type HistoryProvider interface {
	HistorySince(since time.Time) []HistoryEvent
}

// BlameRunner re-runs git blame across the indexed repos and stamps
// meta.last_authored on every blame-eligible node. Returns the number
// of nodes enriched per repo prefix and an aggregate total.
type BlameRunner func() (total int, perRepo map[string]int, err error)

// Options controls what the bundle includes.
type Options struct {
	// Since narrows recent changes. Zero means "last 7 days".
	Since time.Duration
	// Top caps every list section. Zero means 20.
	Top int
	// Sections is an ordered allow-list. Empty means all four:
	// "recent", "ownership", "stale", "blame".
	Sections []string
	// PathPrefix restricts ownership/stale rows to this file prefix.
	PathPrefix string
	// MinSymbols filters ownership table.
	MinSymbols int
	// IncludeBlame triggers BlameRunner. False when blame metadata
	// is already enriched and a re-run isn't desired.
	IncludeBlame bool
	// WorkspaceID restricts nodes to a single workspace. Empty
	// means "all workspaces".
	WorkspaceID string
}

// Bundle is the canonical structured result. RenderMarkdown / RenderJSON
// project it to one of the two wire formats.
type Bundle struct {
	GeneratedAt   time.Time      `json:"generated_at"`
	Since         time.Time      `json:"since"`
	RecentChanges []HistoryEvent `json:"recent_changes,omitempty"`
	OwnershipRows []OwnershipRow `json:"ownership,omitempty"`
	StaleCodeRows []StaleCodeRow `json:"stale_code,omitempty"`
	Blame         *BlameSummary  `json:"blame,omitempty"`
	Sections      []string       `json:"sections"`
}

// OwnershipRow describes one author's stake in the codebase.
type OwnershipRow struct {
	Email   string `json:"email"`
	Symbols int    `json:"symbols"`
	Files   int    `json:"files"`
	Oldest  int64  `json:"oldest_timestamp"`
	Newest  int64  `json:"newest_timestamp"`
}

// StaleCodeRow describes one stale symbol.
type StaleCodeRow struct {
	ID        string `json:"id"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Email     string `json:"email"`
	Commit    string `json:"commit"`
	Timestamp int64  `json:"timestamp"`
	AgeDays   int    `json:"age_days"`
}

// BlameSummary captures the result of a blame re-run.
type BlameSummary struct {
	Enriched int            `json:"enriched"`
	PerRepo  map[string]int `json:"per_repo"`
	Error    string         `json:"error,omitempty"`
}

// Deps bundles the runtime dependencies injected by the MCP/CLI layer.
type Deps struct {
	// Graph is read-only here, so callers may hand over a request-scoped
	// reader (an overlay view) instead of the base store.
	Graph   graph.Reader
	History HistoryProvider
	Blame   BlameRunner
}

// Generate assembles a Bundle by walking the graph and (when
// requested) calling the blame runner. The output is deterministic
// for a fixed graph + clock.
func Generate(deps Deps, opts Options) (*Bundle, error) {
	if deps.Graph == nil {
		return nil, errNilGraph
	}
	if opts.Since == 0 {
		opts.Since = 7 * 24 * time.Hour
	}
	if opts.Top == 0 {
		opts.Top = 20
	}
	if opts.MinSymbols == 0 {
		opts.MinSymbols = 1
	}

	sections := opts.Sections
	if len(sections) == 0 {
		sections = []string{"recent", "ownership", "stale", "blame"}
	}
	want := make(map[string]bool, len(sections))
	for _, s := range sections {
		want[strings.ToLower(strings.TrimSpace(s))] = true
	}

	now := time.Now().UTC()
	since := now.Add(-opts.Since)

	bundle := &Bundle{
		GeneratedAt: now,
		Since:       since,
		Sections:    sections,
	}

	// Recent changes — relies on a watcher being attached.
	if want["recent"] && deps.History != nil {
		evs := deps.History.HistorySince(since)
		if len(evs) > opts.Top {
			evs = evs[len(evs)-opts.Top:]
		}
		bundle.RecentChanges = evs
	}

	// Ownership / stale code — read blame metadata stamped on nodes.
	if want["ownership"] || want["stale"] {
		ownerRows, staleRows := walkNodes(deps.Graph, opts, now)
		if want["ownership"] {
			if len(ownerRows) > opts.Top {
				ownerRows = ownerRows[:opts.Top]
			}
			bundle.OwnershipRows = ownerRows
		}
		if want["stale"] {
			if len(staleRows) > opts.Top {
				staleRows = staleRows[:opts.Top]
			}
			bundle.StaleCodeRows = staleRows
		}
	}

	// Blame re-run (optional; usually run on-demand by the post-commit hook).
	if want["blame"] && deps.Blame != nil && opts.IncludeBlame {
		total, perRepo, err := deps.Blame()
		sum := &BlameSummary{
			Enriched: total,
			PerRepo:  perRepo,
		}
		if err != nil {
			sum.Error = err.Error()
		}
		bundle.Blame = sum
	}

	return bundle, nil
}

// walkNodes does a single pass over symbol nodes and emits the
// ownership and stale-code tables in a single pass.
func walkNodes(g graph.Reader, opts Options, now time.Time) ([]OwnershipRow, []StaleCodeRow) {
	type ownerStats struct {
		row     OwnershipRow
		fileSet map[string]struct{}
	}
	owners := make(map[string]*ownerStats)
	var stale []StaleCodeRow
	cutoffSec := now.Add(-365 * 24 * time.Hour).Unix() // default 365d
	if opts.Since > 0 {
		// stale uses a separate cutoff (always 365d by spec), keep simple.
		cutoffSec = now.Add(-365 * 24 * time.Hour).Unix()
	}

	allowed := map[graph.NodeKind]struct{}{
		graph.KindFunction: {},
		graph.KindMethod:   {},
	}

	for _, n := range g.AllNodes() {
		if _, ok := allowed[n.Kind]; !ok {
			continue
		}
		if !graphpath.HasPrefix(n.FilePath, opts.PathPrefix) {
			continue
		}
		if opts.WorkspaceID != "" {
			ws := n.WorkspaceID
			if ws == "" {
				ws = n.RepoPrefix
			}
			if ws != opts.WorkspaceID {
				continue
			}
		}
		la, ok := n.Meta["last_authored"].(map[string]any)
		if !ok {
			continue
		}
		email, _ := la["email"].(string)
		commit, _ := la["commit"].(string)
		ts := tsFromMeta(la["timestamp"])
		if ts == 0 {
			continue
		}

		if email != "" {
			s, ok := owners[email]
			if !ok {
				s = &ownerStats{
					row:     OwnershipRow{Email: email, Oldest: ts, Newest: ts},
					fileSet: map[string]struct{}{},
				}
				owners[email] = s
			}
			s.row.Symbols++
			s.fileSet[n.FilePath] = struct{}{}
			if ts < s.row.Oldest {
				s.row.Oldest = ts
			}
			if ts > s.row.Newest {
				s.row.Newest = ts
			}
		}

		if ts < cutoffSec {
			stale = append(stale, StaleCodeRow{
				ID:        n.ID,
				File:      n.FilePath,
				Line:      n.StartLine,
				Email:     email,
				Commit:    commit,
				Timestamp: ts,
				AgeDays:   int((now.Unix() - ts) / (24 * 3600)),
			})
		}
	}

	ownerRows := make([]OwnershipRow, 0, len(owners))
	for _, s := range owners {
		if s.row.Symbols < opts.MinSymbols {
			continue
		}
		s.row.Files = len(s.fileSet)
		ownerRows = append(ownerRows, s.row)
	}
	sort.Slice(ownerRows, func(i, j int) bool {
		if ownerRows[i].Symbols != ownerRows[j].Symbols {
			return ownerRows[i].Symbols > ownerRows[j].Symbols
		}
		return ownerRows[i].Email < ownerRows[j].Email
	})
	sort.Slice(stale, func(i, j int) bool {
		return stale[i].Timestamp < stale[j].Timestamp
	})
	return ownerRows, stale
}

// tsFromMeta normalises int64 (in-process) vs float64 (decoded from the
// store's JSON meta) timestamps so this package works on both code paths —
// same trick the MCP handlers use today.
func tsFromMeta(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case int:
		return int64(x)
	}
	return 0
}

// errNilGraph is returned by Generate when no graph is supplied.
type sentinelError string

func (e sentinelError) Error() string { return string(e) }

const errNilGraph sentinelError = "docs: graph is required"
