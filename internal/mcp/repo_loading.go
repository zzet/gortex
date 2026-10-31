package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/indexer"
)

// Startup admission for configured repositories.
//
// The daemon serves its socket long before warmup has registered every
// configured repository. A session whose cwd sits in one of those
// repositories is neither tracked (the registry does not hold it yet) nor
// untracked (the config does), and telling it "run gortex track" sends the
// agent to re-track a repository that is already configured, or to stop.
// These helpers give that window its own answer: `repo_loading`, retryable,
// with progress, and instructions that say tools start working in place.

// ServerInstructionsLoading is the preamble prepended to the initialize
// instructions of a session whose repository is still loading. The rest of
// the instructions are the ordinary ones, because MCP clients read them once
// and the session keeps them after the repository becomes ready.
func ServerInstructionsLoading(repo, phase string, percent int) string {
	if strings.TrimSpace(repo) == "" {
		repo = "this repository"
	}
	return fmt.Sprintf("Gortex is LOADING %s after a restart (%s, %d%%). "+
		"Graph tools return repo_loading (retryable) until it is ready; they start working in place, "+
		"no reconnect is needed. Until then, retry shortly, continue with work that does not need the graph, "+
		"or subscribe to workspace_readiness. Do not re-track it or change tracking configuration: "+
		"the repository is already configured.", repo, loadingPhaseLabel(phase), percent)
}

// ServerInstructionsIdentityPending describes uncertainty, not ownership of
// the caller's path. It is shared by initialization and modern discovery.
func ServerInstructionsIdentityPending() string {
	return serverInstructionsAdmissionPending("identity_resolution", "identity_pending")
}

// ServerInstructionsScopeBindingPending applies after registration when the
// ordinary checkout/scope authority has not established the path's route yet.
func ServerInstructionsScopeBindingPending() string {
	return serverInstructionsAdmissionPending("scope_binding", "scope_binding_pending")
}

func serverInstructionsAdmissionPending(phase, reason string) string {
	return fmt.Sprintf("Gortex is LOADING graph admission after a restart (%s, 0%%). "+
		"Graph admission for this path is not yet established; calls return repo_loading (retryable, %s). "+
		"Retry shortly or continue work natively while admission completes. No graph scope has been granted; "+
		"do not change tracking configuration or reconnect to resolve this temporary state.", phase, reason)
}

// ServerInstructionsUnavailable is the inactive initialize variant for a
// configured repository whose load ended without registering it.
func ServerInstructionsUnavailable(repo, reason string) string {
	if strings.TrimSpace(repo) == "" {
		repo = "this repository"
	}
	return fmt.Sprintf("Gortex is connected but could not load %s after the restart: %s.\n\n"+
		"Graph tools return repo_unavailable for it. To repair, run `gortex track %s` to index it again, "+
		"or `gortex doctor` to diagnose. Until then fall back to your own file reads and text search.",
		repo, reason, repo)
}

// RepoLoadingSuggestion is the remedy text a repo_loading refusal carries. It
// never suggests tracking: the repository is already configured.
func RepoLoadingSuggestion(repo string) string {
	return fmt.Sprintf("Gortex is still loading %s after a restart; tools start working in place, no reconnect needed. "+
		"Retry shortly. Do not change tracking configuration.", repo)
}

// RepoUnavailableSuggestion is the remedy text a repo_unavailable refusal
// carries.
func RepoUnavailableSuggestion(rec indexer.RepoLoad) string {
	if rec.State == indexer.RepoLoadMissing {
		return fmt.Sprintf("The configured path %s does not exist. Restore it, or remove it with `gortex untrack %s`.", rec.Root, rec.Root)
	}
	return fmt.Sprintf("Gortex could not load %s after the restart. Run `gortex track %s` to index it again, or `gortex doctor` to diagnose.", rec.Root, rec.Root)
}

// loadingPhaseLabel names the warmup phase; before the first phase is
// published the daemon is still seeding its checkout catalog.
func loadingPhaseLabel(phase string) string {
	if phase == "" {
		return "starting"
	}
	return phase
}

// WarmupProgress returns the daemon's current warmup phase and its completion
// percentage, as the in-band `warming` block reports them. The phase is
// "starting" before warmup has published one.
func (s *Server) WarmupProgress() (phase string, percent int) {
	w := s.warmupSnapshot()
	return loadingPhaseLabel(w.phase), w.percent
}

// LegacyToolForCall resolves a tools/call to the implementation tool it runs:
// a compact facade call such as workspace(operation:"index") resolves exactly
// as handleFacade would dispatch it (index_health); any other name is
// returned unchanged. An admission allow-list keyed on tool names must be
// checked against this, or the facade spelling slips past it.
func (s *Server) LegacyToolForCall(name string, args map[string]any) string {
	if s == nil || !isFacadeToolName(name) {
		return name
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	if spec, ok := s.viewFacadeOperation(&req); ok && spec.Legacy != "" {
		return spec.Legacy
	}
	return name
}

// PublishRepoReadiness pushes one repository's state change to every
// workspace_readiness subscriber, carrying the current warmup phase so a
// client that treats every event as phase state reads no regression. It does
// not replace the broadcaster's last-known phase.
func (s *Server) PublishRepoReadiness(rec indexer.RepoLoad) {
	if s == nil || s.readinessBroadcaster == nil {
		return
	}
	payload := s.readinessBroadcaster.snapshot()
	if payload == nil {
		payload = map[string]any{}
	}
	delete(payload, "initial_replay")
	payload["event"] = "repo_state"
	payload["repo"] = rec.Prefix
	payload["repo_path"] = rec.Root
	payload["state"] = string(rec.State)
	s.readinessBroadcaster.announce(payload)
}

// NotifySessionMessage sends one notifications/message log entry to a single
// session. Best effort: a session that has gone away is skipped.
func (s *Server) NotifySessionMessage(sessionID, level, message string) {
	if s == nil || s.mcpServer == nil || sessionID == "" {
		return
	}
	if err := s.mcpServer.SendNotificationToSpecificClient(sessionID, "notifications/message", map[string]any{
		"level":  level,
		"logger": "gortex",
		"data":   message,
	}); err != nil && s.logger != nil {
		s.logger.Debug("send notifications/message failed", zap.String("session", sessionID), zap.Error(err))
	}
}

// configuredRepoStillLoading answers a track or index request for a configured
// repository whose warmup job is still queued or running. Re-tracking it would
// queue behind the very warmup it is waiting for and then do nothing; the
// answer says so instead. A repository whose load failed or whose path is
// missing is not short-circuited: tracking it again is the repair.
func (s *Server) configuredRepoStillLoading(ctx context.Context, req mcp.CallToolRequest, target string) (*mcp.CallToolResult, bool) {
	if s == nil || s.multiIndexer == nil || strings.TrimSpace(target) == "" {
		return nil, false
	}
	rec, ok := s.multiIndexer.RepoLoadFor(target)
	if !ok {
		for _, candidate := range s.multiIndexer.RepoLoads() {
			if candidate.Prefix != "" && candidate.Prefix == target {
				rec, ok = candidate, true
				break
			}
		}
	}
	if !ok || !rec.State.Loading() {
		return nil, false
	}
	phase, percent := s.WarmupProgress()
	res, err := s.respondJSONOrTOON(ctx, req, map[string]any{
		"status":  "already_configured",
		"state":   "loading",
		"job":     string(rec.State),
		"path":    rec.Root,
		"repo":    rec.Prefix,
		"phase":   phase,
		"percent": percent,
		"detail": "Gortex is still loading this repository after a restart. It is already configured, so tracking it " +
			"again would not make it load sooner; tools start working in place once it is ready, no reconnect needed.",
	})
	if err != nil {
		return nil, false
	}
	return res, true
}
