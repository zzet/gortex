package resolver

import "github.com/zzet/gortex/internal/graph"

// ResolveFilesAndIncoming runs one scoped cross-repository pass for a complete
// mutation batch. The frontier contains unresolved edges emitted by the changed
// files and unresolved incoming stub buckets for the symbols they define. Both
// directions share one resolver lock and one build of the directory,
// dependency, reachability, and lookup indexes.
func (cr *CrossRepoResolver) ResolveFilesAndIncoming(filePaths []string) *CrossRepoStats {
	return cr.ResolveMutationFiles(filePaths, filePaths)
}

// ResolveMutationFiles separates files that can create or bind unresolved
// edges from files whose resolved base edges only need cross-repository
// materialization. Keeping both roles under one lock still shares the expensive
// workspace indexes without feeding every semantic edge back through resolution.
func (cr *CrossRepoResolver) ResolveMutationFiles(resolutionFiles, materializationFiles []string) *CrossRepoStats {
	return cr.ResolveMutationFrontiers(resolutionFiles, materializationFiles, materializationFiles)
}

// CrossRepoIncomingAdmission is one cross-repository mutation pass's bounded
// incoming-stub admission: the completeness fact plus the typed refusal that
// produced it (*graph.BoundedLocalizationLimitError, nil when nothing was
// refused).
//
// It is returned beside CrossRepoStats rather than folded into it because a
// refused incoming leg is not a count of resolutions — it is the statement that
// this pass never looked at Dropped parked references, and a consumer that
// cannot read it cannot tell a cross-repo pass that had no incoming work from
// one that refused to admit the incoming work it had.
type CrossRepoIncomingAdmission struct {
	graph.IncomingSourceAdmission
	Refusal error
}

// ResolveMutationFrontiers preserves the distinct edge-source and changed-
// definition roles through candidate selection. It discards the incoming
// admission fact after logging it; callers that must react to a bounded pass
// use ResolveMutationFrontiersBounded.
func (cr *CrossRepoResolver) ResolveMutationFrontiers(resolutionFiles, edgeSourceFiles, definitionFiles []string) *CrossRepoStats {
	stats, _ := cr.ResolveMutationFrontiersBounded(resolutionFiles, edgeSourceFiles, definitionFiles)
	return stats
}

// ResolveMutationFrontiersBounded is ResolveMutationFrontiers plus the incoming
// leg's completeness fact. The frontier's incoming admission is charged against
// the same shared ceiling the single-repository legs use (the constant, not one
// budget object: every leg passes a nil budget and gets a fresh one), and a
// refusal empties
// the incoming half of the frontier: the pass then resolves only the changed
// files' own outgoing edges, and its CrossRepoStats — which count exactly that
// — would otherwise read as a complete pass.
func (cr *CrossRepoResolver) ResolveMutationFrontiersBounded(resolutionFiles, edgeSourceFiles, definitionFiles []string) (*CrossRepoStats, CrossRepoIncomingAdmission) {
	stats := &CrossRepoStats{ByRepo: make(map[string]int)}
	var admission CrossRepoIncomingAdmission
	if cr == nil || cr.graph == nil || len(resolutionFiles) == 0 && len(edgeSourceFiles) == 0 && len(definitionFiles) == 0 {
		return stats, admission
	}

	cr.mu.Lock()
	defer cr.mu.Unlock()

	if len(resolutionFiles) > 0 {
		frontier := collectIncrementalFileFrontier(cr.graph, resolutionFiles, nil)
		admission = CrossRepoIncomingAdmission{
			IncomingSourceAdmission: frontier.incomingAdmission,
			Refusal:                 frontier.incomingRefusal,
		}
		logIncomingAdmissionRefusal(cr.logger, "cross_repo_preparation", len(frontier.stubKeys),
			frontier.incomingAdmission, frontier.incomingRefusal)
		pending := make([]*graph.Edge, 0, len(frontier.pending))
		seen := make(map[graph.EdgeIdentity]struct{}, len(frontier.pending))
		for _, edge := range frontier.pending {
			if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
				continue
			}
			identity := graph.EdgeIdentityFor(edge)
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			pending = append(pending, edge)
		}
		if len(pending) > 0 {
			stats = cr.resolveScopedLocked(pending)
		}
	}
	DetectCrossRepoEdgesForMutationFiles(cr.graph, edgeSourceFiles, definitionFiles)
	return stats, admission
}

// DetectCrossRepoEdgesForFiles materializes the cross-repo layer only for base
// edges incident to nodes in the exact changed-file frontier. Inspecting both
// incoming and outgoing edges covers unchanged callers rebound to a changed
// target as well as new calls emitted by the changed source file.
func DetectCrossRepoEdgesForFiles(g graph.Store, filePaths []string) int {
	return DetectCrossRepoEdgesForMutationFiles(g, filePaths, filePaths)
}

// DetectCrossRepoEdgesForMutationFiles materializes resolved base edges using
// the narrow query shape for each mutation role.
func DetectCrossRepoEdgesForMutationFiles(g graph.Store, edgeSourceFiles, definitionFiles []string) int {
	if g == nil || len(edgeSourceFiles) == 0 && len(definitionFiles) == 0 {
		return 0
	}
	return materializeCrossRepoCandidates(g, crossRepoCandidatesForMutationFiles(g, edgeSourceFiles, definitionFiles))
}
