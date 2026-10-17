package resolver

import (
	"sort"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// warmPendingNamesBatch is how many distinct reference targets one warm read
// covers; a deadline is checked between reads.
const warmPendingNamesBatch = 64

// warmPendingNamesNow is the clock WarmPendingNamesUntil checks its deadline
// on.
var warmPendingNamesNow = time.Now

// WarmPendingNames makes, for pending, the name read a resolve pass's lookup
// warm-up makes (warmRepoLanguageNameCache): the same repository/language and
// extern scopes, over g. A store that keeps its answers (a per-file delta's
// per-stack name cache) is thereby warmed for a later pass over those
// references; nothing is resolved or written.
func WarmPendingNames(g graph.Store, pending []*graph.Edge) error {
	_, err := WarmPendingNamesUntil(g, pending, time.Time{})
	return err
}

// WarmPendingNamesUntil is WarmPendingNames read in batches of distinct
// targets, stopping before a batch once deadline (when set) has passed.
// complete reports whether every batch was read.
func WarmPendingNamesUntil(g graph.Store, pending []*graph.Edge, deadline time.Time) (complete bool, err error) {
	if g == nil || len(pending) == 0 {
		return true, nil
	}
	past := func() bool { return !deadline.IsZero() && warmPendingNamesNow().After(deadline) }
	if past() {
		return false, nil
	}
	r := New(g)
	idSet := make(map[string]struct{}, len(pending))
	byTarget := make(map[string][]*graph.Edge)
	for _, edge := range pending {
		if edge == nil {
			continue
		}
		if edge.From != "" {
			idSet[edge.From] = struct{}{}
		}
		byTarget[edge.To] = append(byTarget[edge.To], edge)
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	r.nodeByID = g.GetNodesByIDs(ids)
	if r.nodeByID == nil {
		r.nodeByID = make(map[string]*graph.Node)
	}
	targets := make([]string, 0, len(byTarget))
	for to := range byTarget {
		targets = append(targets, to)
	}
	sort.Strings(targets)
	for start := 0; start < len(targets); start += warmPendingNamesBatch {
		if start > 0 && past() {
			return false, nil
		}
		end := min(start+warmPendingNamesBatch, len(targets))
		var batch []*graph.Edge
		for _, to := range targets[start:end] {
			batch = append(batch, byTarget[to]...)
		}
		if _, _, err := r.warmRepoLanguageNameCache(batch); err != nil {
			return false, err
		}
	}
	return true, nil
}
