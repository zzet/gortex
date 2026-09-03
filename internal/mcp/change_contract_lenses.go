package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zzet/gortex/internal/graph"
)

// This file carries two change sources/lenses that ride the change_contract
// envelope rather than standing up sibling verbs:
//
//   - QRT-3 co-change omissions: files the changed set historically moves with
//     but that are not in the set — "you forgot to update X".
//   - GGR-7 API-drift lens: between two refs (source=diff base=…) with
//     lens=api, focus the verdict on the public surface and its consumers,
//     composing caller analysis + contract drift + covering tests.

const coChangeOmissionThreshold = 0.5

// coChangeOmissions reports files that historically change together with the
// changed set but were left out. File-level co-change edges are mined from git
// history; a high score that is absent from the touched set is a likely
// forgotten companion edit.
func (s *Server) coChangeOmissions(touchedFiles []string) []changeReason {
	if len(touchedFiles) == 0 {
		return nil
	}
	inSet := make(map[string]bool, len(touchedFiles))
	for _, f := range touchedFiles {
		inSet[f] = true
	}
	bestScore := make(map[string]float64)
	for _, f := range touchedFiles {
		for partner, score := range s.coChangeScores(f) {
			if inSet[partner] {
				continue
			}
			if score > bestScore[partner] {
				bestScore[partner] = score
			}
		}
	}

	type omission struct {
		file  string
		score float64
	}
	var oms []omission
	for f, sc := range bestScore {
		if sc >= coChangeOmissionThreshold {
			oms = append(oms, omission{f, sc})
		}
	}
	sort.Slice(oms, func(a, b int) bool {
		if oms[a].score != oms[b].score {
			return oms[a].score > oms[b].score
		}
		return oms[a].file < oms[b].file
	})
	const maxOmissions = 5
	if len(oms) > maxOmissions {
		oms = oms[:maxOmissions]
	}

	var reasons []changeReason
	for _, o := range oms {
		reasons = append(reasons, changeReason{
			Family:     "co_change_omission",
			Severity:   "warn",
			Message:    fmt.Sprintf("%s historically changes with this set (co-change %.2f) but is not included — consider updating it too", o.file, o.score),
			Confidence: o.score,
			Symbol:     o.file,
		})
	}
	return reasons
}

// apiSurfaceEntry describes one exported symbol's exposure for the API lens.
type apiSurfaceEntry struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	File            string   `json:"file"`
	ExternalCallers int      `json:"external_callers"`
	Contracts       []string `json:"contracts,omitempty"`
}

// isExportedChange reports whether a node is part of the public API surface.
// Prefers the indexer-stamped visibility; falls back to the leading-rune
// convention (Go and most curly-brace languages).
func isExportedChange(n *graph.Node) bool {
	if n == nil {
		return false
	}
	if v, ok := n.Meta["visibility"].(string); ok && v != "" {
		switch strings.ToLower(v) {
		case "public", "exported", "open":
			return true
		case "private", "protected", "internal", "package", "package-private", "fileprivate", "unexported":
			return false
		}
	}
	r, _ := utf8.DecodeRuneInString(n.Name)
	return unicode.IsUpper(r)
}

// externalCallers counts call / reference sites that originate outside the
// symbol's own file — the consumers an API change would break. Reads through
// the caller's reader, so an overlay-active request counts the consumers its
// own buffers have.
func externalCallers(r graph.Reader, n *graph.Node) int {
	if r == nil {
		return 0
	}
	count := 0
	for _, e := range r.GetInEdges(n.ID) {
		switch e.Kind {
		case graph.EdgeCalls, graph.EdgeReferences, graph.EdgeCrossRepoCalls:
		default:
			continue
		}
		if caller := r.GetNode(e.From); caller != nil && caller.FilePath != n.FilePath {
			count++
		}
	}
	return count
}

// contractsTouched returns the names of API contracts (HTTP routes, topics,
// …) the symbol participates in, via implements / references edges to
// KindContract nodes. Reads through the caller's reader.
func contractsTouched(r graph.Reader, n *graph.Node) []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]bool)
	var names []string
	visit := func(edges []*graph.Edge, to func(*graph.Edge) string) {
		for _, e := range edges {
			if e.Kind != graph.EdgeImplements && e.Kind != graph.EdgeReferences {
				continue
			}
			if cn := r.GetNode(to(e)); cn != nil && cn.Kind == graph.KindContract {
				if !seen[cn.Name] {
					seen[cn.Name] = true
					names = append(names, cn.Name)
				}
			}
		}
	}
	visit(r.GetOutEdges(n.ID), func(e *graph.Edge) string { return e.To })
	visit(r.GetInEdges(n.ID), func(e *graph.Edge) string { return e.From })
	sort.Strings(names)
	return names
}

// apiDriftReasons evaluates the public-surface drift of the changed set: each
// exported symbol with cross-file consumers (or a participating contract) is a
// breaking-change risk between the two refs. The request reader is resolved
// once and handed to both per-symbol lookups.
func (s *Server) apiDriftReasons(ctx context.Context, p *prediction) ([]changeReason, []apiSurfaceEntry) {
	reader := s.readerFor(ctx)
	var reasons []changeReason
	var surface []apiSurfaceEntry
	for _, n := range p.nodes {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod &&
			n.Kind != graph.KindType && n.Kind != graph.KindInterface {
			continue
		}
		if !isExportedChange(n) {
			continue
		}
		ext := externalCallers(reader, n)
		contracts := contractsTouched(reader, n)
		surface = append(surface, apiSurfaceEntry{
			ID:              n.ID,
			Name:            n.Name,
			Kind:            string(n.Kind),
			File:            n.FilePath,
			ExternalCallers: ext,
			Contracts:       contracts,
		})
		if ext > 0 {
			reasons = append(reasons, changeReason{
				Family:     "api_drift",
				Severity:   "warn",
				Message:    fmt.Sprintf("exported %s %s changed and has %d external caller(s) — a signature change here breaks consumers", n.Kind, n.Name, ext),
				Confidence: 0.8,
				Symbol:     n.ID,
			})
		}
		for _, c := range contracts {
			reasons = append(reasons, changeReason{
				Family:     "contract_drift",
				Severity:   "warn",
				Message:    fmt.Sprintf("%s implements/serves the API contract %q — verify the contract and its mocks still hold", n.Name, c),
				Confidence: 0.75,
				Symbol:     n.ID,
			})
		}
	}
	sort.Slice(surface, func(a, b int) bool {
		if surface[a].ExternalCallers != surface[b].ExternalCallers {
			return surface[a].ExternalCallers > surface[b].ExternalCallers
		}
		return surface[a].Name < surface[b].Name
	})
	return reasons, surface
}
