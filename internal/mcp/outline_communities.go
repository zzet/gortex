package mcp

import "github.com/zzet/gortex/internal/analysis"

const outlineCandidateBatchSize = 128

// outlineCommunitiesInScope batches the next candidates across communities.
// A visible first member needs one node rather than a whole community-sized
// batch. Growing unresolved windows avoids one query per member when a large
// community is entirely outside scope. load shares the request's ID cache.
func outlineCommunitiesInScope(comms []analysis.Community, load func([]string), inScope map[string]bool) []analysis.Community {
	type cursor struct {
		next, end, window int
		done, matched     bool
	}
	cursors := make([]cursor, len(comms))
	pending := 0
	for i, community := range comms {
		cursors[i].window = 1
		cursors[i].done = len(community.Members) == 0
		if !cursors[i].done {
			pending++
		}
	}
	for pending > 0 {
		var ids []string
		for i, community := range comms {
			c := &cursors[i]
			if c.done {
				continue
			}
			c.end = min(c.next+c.window, len(community.Members))
			frontier := community.Members[c.next:c.end]
			for _, id := range frontier {
				if inScope[id] {
					c.done, c.matched = true, true
					pending--
					break
				}
			}
			if !c.done {
				ids = append(ids, frontier...)
			}
		}
		load(ids)
		for i, community := range comms {
			c := &cursors[i]
			if c.done {
				continue
			}
			for _, id := range community.Members[c.next:c.end] {
				if inScope[id] {
					c.matched = true
					break
				}
			}
			c.next = c.end
			if c.matched || c.next == len(community.Members) {
				c.done = true
				pending--
			} else {
				c.window = min(c.window*2, outlineCandidateBatchSize)
			}
		}
	}
	// Completion order can differ from input order. Keep the old order before
	// sorting by size so equal-size communities retain identical top-N output.
	out := make([]analysis.Community, 0, len(comms))
	for i, community := range comms {
		if cursors[i].matched {
			out = append(out, community)
		}
	}
	return out
}
