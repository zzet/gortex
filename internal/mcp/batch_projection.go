package mcp

import (
	"context"
	"github.com/zzet/gortex/internal/pathkey"
	"sync"
	"time"
)

// Preparations live only on the original transaction's stack. The existing
// bounded projection registry is the sole owner of admitted file graphs.
func (s *Server) prepareBatchProjections(ctx context.Context, buffers map[string]*batchFileBuffer, paths []string) map[string]*preparedFileProjection {
	if len(paths) > projectionMaxFiles {
		return nil
	}
	lap := s.beginAcknowledgementLap(ctx, editAckParseGate)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]*preparedFileProjection, len(paths))
	jobs := make(chan int)
	var workers sync.WaitGroup
	var budgetMu sync.Mutex
	var preparedBytes int64
	projectedFiles, exceeded := 0, false
	parsed := false
	for range min(3, len(paths)) {
		workers.Go(func() {
			for {
				select {
				case <-workCtx.Done():
					return
				case i, ok := <-jobs:
					if !ok {
						return
					}
					path := paths[i]
					buffer := buffers[path]
					var candidate *preparedFileProjection
					parserOwned := false
					if buffer.existsAfter {
						built, projection := s.buildFileProjection(workCtx, path, buffer.content)
						candidate = built
						parserOwned = projection != nil
					} else {
						candidate = s.buildRemovalProjection(workCtx, buffer.relPath, path)
					}
					budgetMu.Lock()
					parsed = parsed || parserOwned
					if candidate != nil && candidate.entry != nil {
						preparedBytes += candidate.entry.size
						projectedFiles++
						if projectedFiles > projectionMaxFiles || preparedBytes > projectionMaxBytes {
							exceeded = true
							cancel()
						}
					}
					if !exceeded && workCtx.Err() == nil {
						results[i] = candidate
					}
					budgetMu.Unlock()
				}
			}
		})
	}
feed:
	for i := range paths {
		select {
		case jobs <- i:
		case <-workCtx.Done():
			break feed
		}
	}
	close(jobs)
	workers.Wait() // no cohort worker outlives cancellation or a cap refusal
	lap.applicable = parsed
	lap.finish() // baseline/delta metadata is deliberately outside parse WALL
	if parsed {
		s.noteAcknowledgementParser(ctx, "source_projection_parse_gate")
	} else {
		s.noteAcknowledgementParser(ctx, "not_applicable_text_splice")
	}
	if exceeded || workCtx.Err() != nil {
		return nil
	}
	prepared := make(map[string]*preparedFileProjection, len(paths))
	preparedBytes = 0
	for i, path := range paths {
		if ctx.Err() != nil {
			return nil
		}
		buffer := buffers[path]
		candidate := results[i]
		s.prepareFileViewEvidence(ctx, candidate, &plannedSource{path: path, before: buffer.original, existed: buffer.existsBefore}, buffer.content)
		if candidate != nil && candidate.entry != nil {
			preparedBytes += candidate.entry.size
			if preparedBytes > projectionMaxBytes {
				return nil
			}
		}
		prepared[path] = results[i] // stable original frontier order, independent of workers
	}
	return prepared
}

// Called only after every forward effect and directory sync has succeeded,
// while the original physical union is still held. No sequence enters the
// durable receipt/journal; recovery cannot invent this process-local origin.
func (s *Server) publishBatchProjections(ctx context.Context, state *batchTransactionState, receipt batchTransactionReceipt, prepared map[string]*preparedFileProjection) {
	sequence := mutationDiskCommitSequence.Add(1)
	state.mu.Lock()
	state.projectionCommitSeq = sequence
	state.mu.Unlock()
	members := verifyBatchProjectionCohort(ctx, receipt, prepared, sequence, s.verifyPreparedProjection)
	for _, member := range members {
		holdProjectionForAcknowledgement(projectionAcknowledgementFrom(ctx, s), pendingFileProjections, member.Entry)
	}
	_ = pendingFileProjections.publishCohort(members) // cache refusal never changes disk authority
}

// One deadline covers the complete readback frontier, never the durable writer.
func verifyBatchProjectionCohort(ctx context.Context, receipt batchTransactionReceipt, prepared map[string]*preparedFileProjection, sequence uint64, verify func(context.Context, *preparedFileProjection, uint64, string) (*projectionEntry, error)) []projectionCohortMember {
	verifyCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	members := make([]projectionCohortMember, 0, len(receipt.Files))
	for _, file := range receipt.Files {
		candidate := prepared[file.Path]
		if candidate == nil || candidate.entry == nil {
			continue
		}
		entry, err := verify(verifyCtx, candidate, sequence, file.AfterSHA256)
		if err != nil || verifyCtx.Err() != nil {
			return nil
		}
		members = append(members, projectionCohortMember{Entry: entry, Expected: candidate.expected})
	}
	return members
}

type batchProjectionFileView struct {
	Path            string           `json:"path"`
	ContentSHA256   string           `json:"content_sha256,omitempty"`
	ReadbackExact   bool             `json:"readback_exact"`
	FileView        mutationFileView `json:"file_view"`
	GraphStatusHint string           `json:"graph_status_hint"`
}

type batchProjectionResponse struct {
	batchAcknowledgementResponse
	CommitSeq uint64                    `json:"commit_seq,omitempty"`
	FileViews []batchProjectionFileView `json:"file_views,omitempty"`
}

func (s *Server) batchProjectionResponse(receipt batchTransactionReceipt) batchProjectionResponse {
	response := batchProjectionResponse{batchAcknowledgementResponse: s.batchAcknowledgementResponse(receipt)}
	if value, ok := s.batchTransactions.Load(receipt.TransactionID); ok {
		if state, ok := value.(*batchTransactionState); ok {
			state.mu.RLock()
			response.CommitSeq = state.projectionCommitSeq
			state.mu.RUnlock()
		}
	}
	if receipt.Status != "committed" || receipt.DiskStatus != "committed" {
		return response
	}
	for _, file := range receipt.Files {
		view := batchProjectionFileView{Path: file.Path, ContentSHA256: file.AfterSHA256,
			FileView: mutationFileView{State: "unavailable", Reason: "projection_not_live", DeltaState: "unavailable", DeltaReason: "projection_not_live", SyntaxHealth: fileViewSyntaxHealth{State: "unknown"}}}
		key := pathkey.MutationKey(file.Path)
		entry := pendingFileProjections.snap.Load().entries[key]
		if response.CommitSeq != 0 && entry != nil && entry.commitSeq == response.CommitSeq &&
			entry.contentSHA256 == file.AfterSHA256 && entry.tombstone == file.AfterAbsent && liveProjectionForAcknowledgement(entry) {
			view.ReadbackExact, view.FileView = true, fileViewForEntry(entry)
		}
		view.GraphStatusHint = mutationFileGraphHint(map[string]any{"graph_status": receipt.GraphStatus}, view.ReadbackExact)
		response.FileViews = append(response.FileViews, view)
	}
	return response
}
