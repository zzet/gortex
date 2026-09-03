package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/forge"
)

// ConflictGroup is one merge-order hotspot: a community touched by more
// than one open PR. PRs lists the colliding PR numbers (ascending);
// SuggestedOrder is a safe merge sequence over the same numbers (safest
// PR first); Risk is the merge-conflict-risk score the groups are ranked
// by. Size is the community's member count — a larger shared community is
// a wider blast surface for the overlap.
type ConflictGroup struct {
	Community      string  `json:"community"`
	Size           int     `json:"size"`
	PRs            []int   `json:"prs"`
	SuggestedOrder []int   `json:"suggested_order"`
	Risk           float64 `json:"risk"`
}

// registerConflictsPRTool registers conflicts_prs — a deferred, read-only,
// forge-self-serving tool that surfaces merge-order conflict risk: the
// communities touched by more than one open PR, ranked by how likely a
// merge there is to collide. It maps each open PR's changed files to the
// symbols they define, to the communities those symbols live in, inverts
// to community→PRs, and reports the overlaps. Like the other PR tools it
// accepts caller-supplied data to skip the network and never edits.
func (s *Server) registerConflictsPRTool() {
	s.addTool(
		mcp.NewTool("conflicts_prs",
			mcp.WithDescription("Surface merge-order conflict risk across a repository's open pull requests. Maps each open PR's changed files to the symbols they define, to the graph communities those symbols live in, then reports the communities touched by MORE THAN ONE open PR — the merge-order hotspots where landing PRs in the wrong order is most likely to collide. Each cluster carries the colliding PR numbers, the community size, a suggested safe merge order (lowest-risk PR first), and a conflict-risk score; clusters are ranked highest-risk first. The daemon self-serves the PR list and per-PR files via its forge client (needs GH_TOKEN / GITHUB_TOKEN); pass `prs` and/or `files` to supply already-fetched data and skip the fan-out. Use to plan a merge train that minimises rebases."),
			mcp.WithString("repo", mcp.Description("Repository prefix to resolve the working tree (multi-repo mode).")),
			mcp.WithNumber("limit", mcp.Description("Cap the number of open PRs considered (default 20).")),
			mcp.WithString("prs", mcp.Description("JSON array of already-fetched forge.PR objects to consider instead of listing via the forge.")),
			mcp.WithString("files", mcp.Description("JSON object mapping a PR number (as a string key) to its already-fetched changed file paths, so a per-PR file fetch is skipped.")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleConflictsPRs,
	)
}

// handleConflictsPRs computes the merge-order conflict clusters for a
// repository's open PRs. The PR list and per-PR files come from supplied
// maps or a self-served forge fan-out; the graph join (files→symbols→
// communities) and the conflict grouping are deterministic and pure.
func (s *Server) handleConflictsPRs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError("no graph available — index a repo first"), nil
	}

	repo := strings.TrimSpace(req.GetString("repo", ""))
	limit := req.GetInt("limit", 20)
	if limit < 1 {
		limit = 20
	}

	prs, prsSupplied, err := s.parseSuppliedPRs(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	filesByNumber, err := s.parseSuppliedFilesByNumber(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if !prsSupplied {
		if !forge.Available(ctx) && len(filesByNumber) == 0 {
			return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
		}
		repoRoot, _ := s.diffRepoScope(ctx, repo)
		fetched, ferr := forgeList(ctx, repoRoot, forge.ListOpts{State: "open", Limit: limit, WithCI: true})
		if ferr != nil {
			if errors.Is(ferr, forge.ErrRateLimited) {
				return s.respondJSONOrTOON(ctx, req, rateLimitedPayload(ferr))
			}
			if isForgeUnavailable(ferr) {
				return s.respondJSONOrTOON(ctx, req, forgeUnavailablePayload())
			}
			return mcp.NewToolResultError(fmt.Sprintf("listing PRs failed: %v", ferr)), nil
		}
		prs = fetched
		for _, pr := range prs {
			s.prCache.putList(repo, pr.Number, pr)
		}
	}

	if limit > 0 && len(prs) > limit {
		prs = prs[:limit]
	}

	communities := s.getCommunities()
	var nodeToComm map[string]string
	commSizes := map[string]int{}
	if communities != nil {
		nodeToComm = communities.NodeToComm
		for _, c := range communities.Communities {
			commSizes[c.ID] = c.Size
		}
	}

	// Per-PR community fan-out and a per-PR risk score for the suggested
	// merge order. Both derive from the PR's changed-file set.
	joinPrefix := s.prJoinPrefix(ctx, repo)
	prCommunities := map[int][]string{}
	prRisk := map[int]float64{}
	for _, pr := range prs {
		files, degraded, ferr := s.resolvePRFiles(ctx, repo, pr, filesByNumber)
		if degraded != nil {
			return s.respondJSONOrTOON(ctx, req, degraded)
		}
		if ferr != nil {
			return mcp.NewToolResultError(ferr.Error()), nil
		}

		changedFiles, changedSymbolNodes := s.changedSymbolsForFiles(ctx, joinPrefix, files)
		symbolIDs := make([]string, 0, len(changedSymbolNodes))
		for _, n := range changedSymbolNodes {
			symbolIDs = append(symbolIDs, n.ID)
		}

		commSet := map[string]bool{}
		for _, id := range symbolIDs {
			if cid, ok := nodeToComm[id]; ok && cid != "" {
				commSet[cid] = true
			}
		}
		comms := make([]string, 0, len(commSet))
		for c := range commSet {
			comms = append(comms, c)
		}
		prCommunities[pr.Number] = comms

		result := analysis.ScorePRRisk(s.readerFor(ctx), analysis.PRRiskInput{
			SymbolIDs:    symbolIDs,
			ChangedFiles: changedFiles,
			NodeToComm:   nodeToComm,
			Communities:  communities,
			Processes:    s.getProcesses(),
		})
		prRisk[pr.Number] = result.Score
	}

	groups := communityConflicts(prCommunities, commSizes, prRisk)

	rows := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		rows = append(rows, map[string]any{
			"community":       g.Community,
			"size":            g.Size,
			"prs":             g.PRs,
			"suggested_order": g.SuggestedOrder,
			"risk":            g.Risk,
		})
	}
	payload := map[string]any{
		"conflicts": rows,
		"total":     len(rows),
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeConflictsPRs(payload))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// communityConflicts is the pure core of conflicts_prs. It inverts a
// per-PR community fan-out (prCommunities[prNumber] = communities the PR
// touches) into the communities touched by MORE THAN ONE PR — the merge-
// order hotspots — and projects each onto a ConflictGroup.
//
//   - commSizes maps a community id to its member count; a missing entry
//     is treated as size 0.
//   - prRisk maps a PR number to its composite change-risk score; it
//     drives the suggested merge order (lowest risk merges first, safest-
//     first) and a missing entry is treated as 0.
//
// The returned groups are ranked highest-conflict-risk first. Risk is a
// monotone blend of the colliding-PR count (dominant) and the community
// size, so a community touched by three PRs always outranks one touched
// by two regardless of size, and ties break on size then community id.
// Everything — PR lists, suggested order, and group order — is fully
// deterministic. Communities touched by at most one PR are omitted.
func communityConflicts(prCommunities map[int][]string, commSizes map[string]int, prRisk map[int]float64) []ConflictGroup {
	// Invert: community → set of PR numbers touching it.
	commToPRs := map[string]map[int]bool{}
	for pr, comms := range prCommunities {
		for _, c := range comms {
			if c == "" {
				continue
			}
			if commToPRs[c] == nil {
				commToPRs[c] = map[int]bool{}
			}
			commToPRs[c][pr] = true
		}
	}

	groups := make([]ConflictGroup, 0, len(commToPRs))
	for comm, prSet := range commToPRs {
		if len(prSet) < 2 {
			continue
		}
		prs := make([]int, 0, len(prSet))
		for pr := range prSet {
			prs = append(prs, pr)
		}
		sort.Ints(prs)

		// Suggested merge order: lowest-risk PR first (safest to land
		// early), PR number ascending on a tie. A fresh slice — never the
		// ascending-number PRs slice aliased.
		order := append([]int(nil), prs...)
		sort.SliceStable(order, func(i, j int) bool {
			ri, rj := prRisk[order[i]], prRisk[order[j]]
			if ri != rj {
				return ri < rj
			}
			return order[i] < order[j]
		})

		size := commSizes[comm]
		groups = append(groups, ConflictGroup{
			Community:      comm,
			Size:           size,
			PRs:            prs,
			SuggestedOrder: order,
			Risk:           conflictRisk(len(prs), size),
		})
	}

	// Rank: PR count descending (dominant), then community size
	// descending, then community id ascending for a total order.
	sort.SliceStable(groups, func(i, j int) bool {
		if len(groups[i].PRs) != len(groups[j].PRs) {
			return len(groups[i].PRs) > len(groups[j].PRs)
		}
		if groups[i].Size != groups[j].Size {
			return groups[i].Size > groups[j].Size
		}
		return groups[i].Community < groups[j].Community
	})

	return groups
}

// conflictRisk blends the colliding-PR count and the community size into
// a single merge-conflict-risk score. The PR count dominates: each extra
// PR adds a whole point, while the size only contributes a sub-unit
// fraction that saturates as the community grows — so a 3-PR community
// always outscores a 2-PR one no matter the sizes, and among equal-count
// communities the larger (wider blast surface) ranks higher.
func conflictRisk(prCount, size int) float64 {
	if prCount < 2 {
		return 0
	}
	sizeFactor := float64(size) / (float64(size) + 10.0)
	return float64(prCount) + sizeFactor
}
