package resolver

import "time"

// ResolveFilesAndIncomingObserved reports this execution's actual mutex
// acquisition separately from module refresh and resolution. A nil observer
// retains the legacy path; an empty execution makes no observation. The
// callback runs under ResolveMutex and must be bounded and non-reentrant.
func (r *Resolver) ResolveFilesAndIncomingObserved(files []string, observe func(started, acquired time.Time)) *ResolveStats {
	return r.resolveFilesAndIncoming(files, observe, 0)
}

// ResolveFilesAndIncomingObservedWithTimeout is ResolveFilesAndIncomingObserved
// with a bounded contention wait window applied strictly under r.mu for
// opportunistic catchup passes so unrelated background retries cannot stall an
// interactive save on a contended SQLite writer gate.
func (r *Resolver) ResolveFilesAndIncomingObservedWithTimeout(files []string, observe func(started, acquired time.Time), maxWait time.Duration) *ResolveStats {
	return r.resolveFilesAndIncoming(files, observe, maxWait)
}

// ResolveFilesAndIncoming runs the legacy unobserved execution.
func (r *Resolver) ResolveFilesAndIncoming(files []string) *ResolveStats {
	return r.resolveFilesAndIncoming(files, nil, 0)
}

// ResolveMutationFrontiersObserved observes the scoped cross-repository
// execution's mutex acquisition. The callback has the same restrictions as
// ResolveFilesAndIncomingObserved; it is never installed on a shared resolver.
func (cr *CrossRepoResolver) ResolveMutationFrontiersObserved(resolutionFiles, edgeSourceFiles, definitionFiles []string, observe func(time.Time, time.Time)) *CrossRepoStats {
	stats, _ := cr.resolveMutationFrontiersBounded(resolutionFiles, edgeSourceFiles, definitionFiles, observe, 0)
	return stats
}

// ResolveMutationFrontiersObservedWithTimeout is ResolveMutationFrontiersObserved
// with a bounded contention wait window applied strictly under cr.mu for
// opportunistic catchup passes.
func (cr *CrossRepoResolver) ResolveMutationFrontiersObservedWithTimeout(resolutionFiles, edgeSourceFiles, definitionFiles []string, observe func(time.Time, time.Time), maxWait time.Duration) *CrossRepoStats {
	stats, _ := cr.resolveMutationFrontiersBounded(resolutionFiles, edgeSourceFiles, definitionFiles, observe, maxWait)
	return stats
}
