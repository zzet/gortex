package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/pathkey"
)

// repoPendingAdmission is cwdReachableChecked's verdict for a cwd in a
// configured repository the registry does not hold yet. It travels as an
// error so the reachability signature stays (bool, error), but it is not a
// failure of the check: Dispatch unwraps it and answers the session with the
// repository's load state instead of treating the cwd as untracked.
type repoPendingAdmission struct {
	load               indexer.RepoLoad
	familyInstructions string
}

func (a *repoPendingAdmission) Error() string {
	return fmt.Sprintf("repository %s is %s", a.load.Root, a.load.State)
}

// pendingRepoRefusal decides one frame from a session whose repository is not
// registered yet. It returns the refusal to send, or nil to let the frame
// through to the server.
//
// While the repository is loading, the connection metadata and the tools that
// explain or wait for the load answer; every graph-backed call, resource read
// and mutation gets repo_loading. Once the load has ended without registering,
// the session gets the untracked rules — capabilities and track still answer,
// because tracking again is the repair — with repo_unavailable and the reason
// instead of repo_not_tracked.
func (d *mcpDispatcher) pendingRepoRefusal(sess *daemon.Session, frame []byte, pending *repoPendingAdmission) []byte {
	rec := pending.load
	method := peekFrameMethod(frame)
	if pending.familyInstructions != "" {
		if method == "tools/call" && !untrackedBootstrapCall(frame) {
			return familyDiscoveryError(sess.CWD, frame, "family_pending")
		}
		return nil
	}
	if !rec.State.Loading() {
		if method == "tools/call" && !untrackedBootstrapCall(frame) {
			return repoUnavailableError(sess, frame, rec)
		}
		return nil
	}
	switch {
	case method == "tools/call":
		name, args := peekFrameToolCall(frame)
		legacy := name
		if d.srv != nil {
			legacy = d.srv.LegacyToolForCall(name, args)
		}
		switch {
		case legacy == "index_health":
			return d.loadingIndexHealth(frame, rec)
		case loadingAdmittedTool(legacy):
			if rec.State == indexer.RepoLoadIdentityPending && (legacy == "track_repository" || legacy == "index_repository") {
				return d.repoLoadingError(sess, frame, rec)
			}
			return nil
		default:
			return d.repoLoadingError(sess, frame, rec)
		}
	case loadingMetadataMethod(method):
		return nil
	default:
		return d.repoLoadingError(sess, frame, rec)
	}
}

// loadingAdmittedTool names the implementation tools a loading session may
// call. They read no repository graph: the static catalog, the readiness
// subscriptions, and track/index, whose handlers answer already_configured
// for a repository that is still loading. graph_stats is not here: it reads
// the store and can block behind warmup, which is the wait it would report on.
func loadingAdmittedTool(legacy string) bool {
	switch legacy {
	case "capabilities", "tools_search", "track_repository", "index_repository":
		return true
	}
	return strings.HasPrefix(legacy, "subscribe_") || strings.HasPrefix(legacy, "unsubscribe_")
}

// loadingMetadataMethod is the connection metadata a loading session keeps:
// the handshake, the catalog listings and liveness. None of it reads a graph.
func loadingMetadataMethod(method string) bool {
	switch method {
	case "resources/list", "resources/templates/list", "prompts/list":
		return true
	}
	return checkoutAdmissionMetadataMethod(method)
}

// peekFrameToolCall returns the tool name and arguments of a tools/call frame.
func peekFrameToolCall(frame []byte) (string, map[string]any) {
	var peek struct {
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &peek) != nil {
		return "", nil
	}
	return peek.Params.Name, peek.Params.Arguments
}

// repoLoadingError is the refusal for a graph-backed frame from a loading
// session. Retryable, with progress, and it never suggests tracking.
func (d *mcpDispatcher) repoLoadingError(sess *daemon.Session, inbound []byte, rec indexer.RepoLoad) []byte {
	if rec.State == indexer.RepoLoadIdentityPending {
		reason, phase, suggestion := "identity_pending", "identity_resolution", gortexmcp.ServerInstructionsIdentityPending()
		if rec.Reason == "scope_binding_pending" {
			reason, phase, suggestion = rec.Reason, "scope_binding", gortexmcp.ServerInstructionsScopeBindingPending()
		}
		return jsonRPCErrorFrame(inbound, "graph admission is pending", map[string]any{
			"error_code": string(gortexmcp.ErrCodeRepoLoading), "retryable": true, "path": sess.CWD,
			"state": reason, "reason": reason, "phase": phase, "percent": 0,
			"suggestion": suggestion,
		})
	}
	phase, percent := "starting", 0
	if d.srv != nil {
		phase, percent = d.srv.WarmupProgress()
	}
	label := rec.Label()
	return jsonRPCErrorFrame(inbound, fmt.Sprintf("repository loading: %s (%s, %d%%)", label, phase, percent), map[string]any{
		"error_code": string(gortexmcp.ErrCodeRepoLoading),
		"retryable":  true,
		"path":       sess.CWD,
		"repo":       rec.Prefix,
		"repo_path":  rec.Root,
		"state":      string(rec.State),
		"phase":      phase,
		"percent":    percent,
		"suggestion": gortexmcp.RepoLoadingSuggestion(label),
	})
}

// repoUnavailableError is the refusal for a session whose configured
// repository's load ended without registering it.
func repoUnavailableError(sess *daemon.Session, inbound []byte, rec indexer.RepoLoad) []byte {
	if rec.ErrorCode == daemon.ErrPrefixCollision {
		return jsonRPCErrorFrame(inbound, "repository prefix collision: "+rec.Reason, map[string]any{
			"error_code": daemon.ErrPrefixCollision, "retryable": false,
			"path": sess.CWD, "repo_path": rec.Root, "prefix": rec.Prefix,
			"owner_path": rec.OwnerPath, "owner_state": rec.OwnerState,
			"suggestion": gortexmcp.PrefixCollisionSuggestion(rec),
		})
	}
	return jsonRPCErrorFrame(inbound, fmt.Sprintf("repository unavailable: %s (%s)", rec.Label(), rec.Reason), map[string]any{
		"error_code": string(gortexmcp.ErrCodeRepoUnavailable),
		"retryable":  false,
		"path":       sess.CWD,
		"repo":       rec.Prefix,
		"repo_path":  rec.Root,
		"state":      string(rec.State),
		"reason":     rec.Reason,
		"suggestion": gortexmcp.RepoUnavailableSuggestion(rec),
	})
}

// loadingIndexHealth answers index_health for a loading session from the load
// record alone. The daemon-wide health snapshot names every repository; an
// unbound session must see only its own, so the answer is built here rather
// than filtered out of that snapshot.
func (d *mcpDispatcher) loadingIndexHealth(inbound []byte, rec indexer.RepoLoad) []byte {
	phase, percent := "starting", 0
	if d.srv != nil {
		phase, percent = d.srv.WarmupProgress()
	}
	repos := []map[string]any{}
	message := gortexmcp.ServerInstructionsIdentityPending()
	reason := ""
	if rec.State == indexer.RepoLoadIdentityPending {
		phase, percent, reason = "identity_resolution", 0, "identity_pending"
		if rec.Reason == "scope_binding_pending" {
			phase, reason, message = "scope_binding", rec.Reason, gortexmcp.ServerInstructionsScopeBindingPending()
		}
	} else {
		repos = append(repos, map[string]any{"repo": rec.Prefix, "path": rec.Root, "state": string(rec.State), "since": rec.Since})
		message = gortexmcp.RepoLoadingSuggestion(rec.Label())
	}
	payload, _ := json.Marshal(map[string]any{
		"status": "loading", "phase": phase, "percent": percent, "repos": repos, "message": message, "reason": reason,
	})
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(inbound, &req)
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(payload)}},
		},
	})
	return out
}

// jsonRPCErrorFrame builds a -32000 server error paired to inbound's id.
func jsonRPCErrorFrame(inbound []byte, message string, data map[string]any) []byte {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(inbound, &req)
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"error": map[string]any{
			"code":    -32000,
			"message": message,
			"data":    data,
		},
	})
	return out
}

// notePendingRepoSession remembers a session whose initialize the server
// answered with loading instructions (stateAwareInstructions decides the
// instructions from the same load state this gate reads), so the session is
// told once its repository is ready.
func (d *mcpDispatcher) notePendingRepoSession(sess *daemon.Session, method string, rec indexer.RepoLoad) {
	if (method != "initialize" && method != "server/discover") || !rec.State.Loading() || sess == nil || sess.ID == "" {
		return
	}
	d.loadingSessions.Store(sess.ID, rec)
}

// notifyRepoReady tells every session whose initialize reported rec as loading
// that it is ready now. Each session is told once.
func (d *mcpDispatcher) notifyRepoReady(rec indexer.RepoLoad) {
	if d == nil || d.srv == nil {
		return
	}
	message := fmt.Sprintf("Gortex finished loading %s; graph tools answer for it now in this session, no reconnect needed.", rec.Label())
	d.loadingSessions.Range(func(key, value any) bool {
		told, _ := value.(indexer.RepoLoad)
		if !pathkey.EqualPaths(told.Root, rec.Root) {
			return true
		}
		d.loadingSessions.Delete(key)
		if id, _ := key.(string); id != "" {
			d.srv.NotifySessionMessage(id, "info", message)
		}
		return true
	})
}
