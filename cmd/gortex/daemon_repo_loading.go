package main

import (
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

// listenConfiguredDaemon publishes lexical configured roots before opening
// either listener. It performs no filesystem validation of those roots; warmup
// does that later so a hung mount cannot delay serving other configured repos.
func listenConfiguredDaemon(srv *daemon.Server, state *daemonState) error {
	if state != nil && state.multiIndexer != nil && state.configManager != nil {
		if gc := state.configManager.Global(); gc != nil {
			state.multiIndexer.DeclareRepos(gc.ReposSnapshot())
		}
	}
	return srv.Listen()
}

// announceConfiguredRepos fills the declarations' filesystem identity before
// checkout seeding. Serving has already started, so filesystem work stays here
// rather than in the lexical declaration that precedes Listen.
func announceConfiguredRepos(state *daemonState) {
	if state == nil || state.multiIndexer == nil || state.configManager == nil {
		return
	}
	if gc := state.configManager.Global(); gc != nil {
		state.multiIndexer.ExpectRepos(gc.ReposSnapshot())
	}
}

// installRepoLoadTracking wires what has to happen when a configured
// repository becomes ready.
func installRepoLoadTracking(state *daemonState, disp *mcpDispatcher) {
	if state == nil || state.multiIndexer == nil {
		return
	}
	state.multiIndexer.OnRepoRegistered(func(rec indexer.RepoLoad) {
		if state.mcpServer != nil {
			// A session that resolved its scope while the repository was not
			// registered — through the router arm, or a resources/read — holds
			// a scope that sees nothing. Drop every cached scope so the next
			// call binds to the repository, with no reconnect.
			state.mcpServer.InvalidateSessionScopes()
			state.mcpServer.PublishRepoReadiness(rec)
		}
		disp.notifyRepoReady(rec)
		if state.lifecycle != nil {
			state.lifecycle.PrimaryRegistered(rec.Prefix)
		}
	})
}

// publicRepoState is the wire spelling of a load state: warmup's queued and
// running both read as loading to a client.
func publicRepoState(state indexer.RepoLoadState) string {
	switch {
	case state.Loading():
		return daemon.RepoStateLoading
	case state.Ready():
		return daemon.RepoStateReady
	case state == indexer.RepoLoadMissing:
		return daemon.RepoStateMissing
	case state == indexer.RepoLoadFailed:
		return daemon.RepoStateFailed
	}
	return ""
}

// pendingRepoLoadForPath reads one snapshot: ownership and unresolved identity
// must never be separated by a concurrent warmup publication. Ready records
// still require ordinary coverage authority, not permission from this table.
func (c *realController) pendingRepoLoadForPath(abs string) (indexer.RepoLoad, bool) {
	if c == nil || c.multiIndexer == nil {
		return indexer.RepoLoad{}, false
	}
	return c.multiIndexer.RepoLoadAdmissionForPath(abs)
}
