package mcp

import (
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// Guard rules are declared per repo, in that repo's `.gortex.yaml`, but
// Server.guardRules is a single process-level snapshot taken at
// construction. In the daemon that snapshot comes from whatever
// `.gortex.yaml` happened to sit in the daemon's own working directory —
// which for a multi-repo daemon governs none of the tracked repos, and for
// a daemon started outside the repo governs nothing at all. That is why a
// daemon could honour a repo's excludes on the indexer walk (the walk
// resolves config per repo through the ConfigManager) while check_guards
// on the same daemon answered "no guard rules configured".
//
// The ConfigManager holds the real per-repo config, keyed by repo prefix,
// so resolve through it and keep the process-level snapshot only as the
// fallback for servers wired without one (embedded single-repo mode,
// tests).

// guardRulesFor returns the guard rules governing one repo prefix.
func (s *Server) guardRulesFor(repoPrefix string) []config.GuardRule {
	if s.configManager != nil {
		if rules := s.configManager.EffectiveGuardRules(repoPrefix); len(rules) > 0 {
			return rules
		}
	}
	return s.guardRules
}

// repoScopeResolver maps node IDs to the repo prefix whose config governs
// them. repoPrefixOf alone will not do: it returns the leading path
// segment, which in a solo (unprefixed) repo is a source directory rather
// than a repo — "pkg/handler/h.go::Handle" would resolve to repo "pkg".
// Checking the segment against the set of repos actually tracked in this
// process tells the two apart, and a single tracked repo absorbs every ID
// that names no repo of its own.
type repoScopeResolver struct {
	tracked map[string]struct{}
	sole    string
}

// newRepoScopeResolver collects the tracked repo prefixes from both
// sources that know them: the ConfigManager (every repo whose workspace
// config was loaded) and the MultiIndexer (every repo registered on the
// graph). Either may be absent depending on how the server was wired.
func (s *Server) newRepoScopeResolver() repoScopeResolver {
	r := repoScopeResolver{tracked: make(map[string]struct{}, 4)}
	for _, prefix := range s.configManager.WorkspacePrefixes() {
		if prefix != "" {
			r.tracked[prefix] = struct{}{}
		}
	}
	if s.multiIndexer != nil {
		for prefix := range s.multiIndexer.AllMetadata() {
			if prefix != "" {
				r.tracked[prefix] = struct{}{}
			}
		}
	}
	if len(r.tracked) == 1 {
		for prefix := range r.tracked {
			r.sole = prefix
		}
	}
	return r
}

// prefixOf returns the repo prefix governing one node ID.
func (r repoScopeResolver) prefixOf(id string) string {
	p := repoPrefixOf(id)
	if p == "" {
		return r.sole
	}
	if _, ok := r.tracked[p]; ok {
		return p
	}
	if len(r.tracked) == 0 {
		// Nothing to check against (embedded mode, tests): the ID's own
		// leading segment is the best information available.
		return p
	}
	// Not a tracked repo — an unprefixed ID from a solo repo.
	return r.sole
}

// groupIDsByRepo buckets node IDs by the repo prefix whose config governs
// them, returning the buckets plus the prefixes in first-seen order so
// results stay deterministic.
func (s *Server) groupIDsByRepo(ids []string) (map[string][]string, []string) {
	resolver := s.newRepoScopeResolver()
	groups := make(map[string][]string, 2)
	order := make([]string, 0, 2)
	for _, id := range ids {
		prefix := resolver.prefixOf(id)
		if _, seen := groups[prefix]; !seen {
			order = append(order, prefix)
		}
		groups[prefix] = append(groups[prefix], id)
	}
	return groups, order
}

// evaluateGuards evaluates each ID against the guard rules of its OWN
// repo. Evaluating the union of every repo's rules over every ID would let
// a rule declared in one repo fire against another repo's symbols, so the
// IDs are grouped by repo first. Rule evaluation only reads, so it runs on
// the caller's reader and an overlay-active request is gated on the buffers
// it pushed.
func (s *Server) evaluateGuards(r graph.Reader, ids []string) []analysis.GuardViolation {
	if len(ids) == 0 {
		return nil
	}
	groups, order := s.groupIDsByRepo(ids)
	var out []analysis.GuardViolation
	for _, prefix := range order {
		rules := s.guardRulesFor(prefix)
		if len(rules) == 0 {
			continue
		}
		out = append(out, analysis.EvaluateGuards(r, rules, groups[prefix])...)
	}
	return out
}

// hasGuardRules reports whether any guard rule could fire for these IDs.
// The cheap gate in front of evaluateGuards, and the check behind the
// "no guard rules configured" answer.
func (s *Server) hasGuardRules(ids []string) bool {
	if len(s.guardRules) > 0 {
		return true
	}
	if s.configManager == nil {
		return false
	}
	_, order := s.groupIDsByRepo(ids)
	for _, prefix := range order {
		if len(s.configManager.EffectiveGuardRules(prefix)) > 0 {
			return true
		}
	}
	return false
}

// anyGuardRulesConfigured reports whether any TRACKED repo declares guard
// rules, independent of a particular ID set. Used where there is no
// changed-symbol set to scope by.
func (s *Server) anyGuardRulesConfigured() bool {
	if len(s.guardRules) > 0 {
		return true
	}
	if s.configManager == nil {
		return false
	}
	for prefix := range s.newRepoScopeResolver().tracked {
		if len(s.configManager.EffectiveGuardRules(prefix)) > 0 {
			return true
		}
	}
	return false
}

// guardsFamily adapts the per-repo guard resolution to
// analysis.RuleFamily, so the change-contract pipeline scopes rules to
// each changed symbol's repo exactly like check_guards does. A family
// carrying one flat rule list cannot express that per-repo scoping.
type guardsFamily struct{ srv *Server }

func (f guardsFamily) Name() string { return "guards" }

func (f guardsFamily) Evaluate(g graph.Reader, changedSet []string) []analysis.GuardViolation {
	return f.srv.evaluateGuards(g, changedSet)
}
