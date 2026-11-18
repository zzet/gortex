package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

const trackOutcomeRetention = 10 * time.Minute

type repositoryTrackOutcome struct {
	result indexer.RegisterResult
	err    error
}

func snapshotTrackOutcome(result indexer.RegisterResult, err error) repositoryTrackOutcome {
	// Keep only wire-visible scalars. The original index result can retain
	// derivation capabilities that have no role in a ten-minute retry cache.
	out := indexer.RegisterResult{Prefix: result.Prefix, AlreadyTracked: result.AlreadyTracked}
	if result.Index != nil {
		out.Index = &indexer.IndexResult{FileCount: result.Index.FileCount, NodeCount: result.Index.NodeCount, EdgeCount: result.Index.EdgeCount}
	}
	return repositoryTrackOutcome{result: out, err: err}
}

// Closing done publishes the immutable final outcome and completion time.
// Running jobs never expire. There is no timer or background cache producer.
type repositoryTrackJob struct {
	done      chan struct{}
	outcome   repositoryTrackOutcome
	completed time.Time
	witness   indexer.TrackOutcomeWitness
}

type repositoryTrackKey struct {
	root       string
	name       string
	asWorktree bool
	force      bool
}

func trackRequestKey(root string, entry config.RepoEntry) repositoryTrackKey {
	return repositoryTrackKey{root: root, name: entry.Name, asWorktree: entry.AsWorktree, force: entry.Force}
}

func (s *Server) storedTrackOutcome(ctx context.Context, req mcp.CallToolRequest, path string, entry config.RepoEntry, key repositoryTrackKey) (*mcp.CallToolResult, error, bool) {
	cached, ok := s.trackInFlight.Load(key)
	if !ok {
		return nil, nil, false
	}
	job := cached.(*repositoryTrackJob)
	select {
	case <-job.done:
		if time.Since(job.completed) < trackOutcomeRetention && s.multiIndexer.TrackOutcomeCurrent(job.witness) {
			result, err := s.trackRepositoryOutcome(ctx, req, path, entry, job.outcome, true)
			return result, err, true
		}
		s.trackInFlight.CompareAndDelete(key, job)
		return nil, nil, false
	default:
		result, err := s.trackRepositoryAccepted(ctx, req, path)
		return result, err, true
	}
}

func (s *Server) expireTrackOutcomes(now time.Time) {
	s.trackInFlight.Range(func(key, value any) bool {
		job := value.(*repositoryTrackJob)
		select {
		case <-job.done:
			if now.Sub(job.completed) >= trackOutcomeRetention {
				s.trackInFlight.CompareAndDelete(key, job)
			}
		default:
		}
		return true
	})
}

func (s *Server) trackRepositoryAccepted(ctx context.Context, req mcp.CallToolRequest, path string) (*mcp.CallToolResult, error) {
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"status": "accepted", "path": path,
		"detail": "an initial index for this path is already running; call track again later to read its result",
	})
}

func (s *Server) trackRepositoryOutcome(ctx context.Context, req mcp.CallToolRequest, path string, entry config.RepoEntry, out repositoryTrackOutcome, cached bool) (*mcp.CallToolResult, error) {
	if out.err != nil {
		if errors.Is(out.err, indexer.ErrRepoIdentityPending) {
			payload := map[string]any{
				"error_code": string(ErrCodeRepoLoading), "retryable": true,
				"reason": "identity_pending", "phase": "identity_resolution", "percent": 0,
				"path": path, "suggestion": ServerInstructionsIdentityPending(),
			}
			result, err := s.respondJSONOrTOON(ctx, req, payload)
			if result != nil {
				result.IsError, result.StructuredContent = true, payload
			}
			return result, err
		}
		var collision *indexer.PrefixCollisionError
		if !errors.As(out.err, &collision) {
			return mcp.NewToolResultError(out.err.Error()), nil
		}
		message := fmt.Sprintf("repository prefix %q is already used by %s; for a new path set the name parameter to a different prefix for %s; for an already configured path set name: in the global config and run `gortex daemon reload`",
			collision.Prefix, collision.ExistingPath, collision.Path)
		if collision.OwnerMissing {
			message = fmt.Sprintf("repository prefix %q is already used by %s, which no longer exists; inspect untrack_repository for that owner path before tracking %s again", collision.Prefix, collision.ExistingPath, collision.Path)
		}
		payload := map[string]any{
			"error_code": "prefix_collision", "message": message,
			"prefix": collision.Prefix, "owner_path": collision.ExistingPath,
			"owner_state": collision.OwnerState,
		}
		result, err := mcp.NewToolResultJSON(payload)
		if err != nil {
			return nil, err
		}
		result.IsError = true
		result.StructuredContent = payload
		return result, nil
	}
	if out.result.AlreadyTracked {
		return mcp.NewToolResultText("repository already tracked"), nil
	}
	prefix := out.result.Prefix
	if prefix == "" {
		prefix = config.ResolvePrefix(entry)
	}
	payload := map[string]any{
		"status": "tracked", "path": path, "prefix": prefix,
		"file_count": out.result.Index.FileCount, "node_count": out.result.Index.NodeCount,
		"edge_count": out.result.Index.EdgeCount,
	}
	if cached {
		payload["detail"] = "repository already tracked; returning the stored final track outcome"
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}
