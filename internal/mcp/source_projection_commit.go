package mcp

import (
	"context"
	"path/filepath"
	"weak"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/pathkey"
)

// Only the bounded registry strongly owns accepted file graphs. A physical
// receipt retains a weak link and scalars, so rejected/retired projections
// cannot escape the registry's global memory allowance through the ledger.
func (s *Server) publishCommittedFileProjection(ctx context.Context, record *mutationCommitRecord, prepared *preparedFileProjection) {
	if prepared == nil {
		return
	}
	reason := prepared.refusal
	entry := prepared.entry
	exact := false
	if entry != nil {
		reason = "readback_unavailable"
		if pathkey.MutationKey(entry.absPath) == prepared.key {
			before, err := statDiskStamp(entry.absPath)
			if entry.tombstone {
				// Absence is exact readback of this held-path unlink, not proof
				// that any background generation has published the deletion.
				if err == nil && before.info == nil && pathkey.MutationKey(entry.absPath) == prepared.key {
					exact = true
					reason = publishPhysicalProjection(record, entry, before)
				}
			} else if err == nil && before.info != nil && before.info.Mode().IsRegular() {
				files, err := source.NewFilesystemSource(filepath.Dir(entry.absPath))
				if err == nil {
					raw, _, readErr := files.ReadRegularFile(ctx, filepath.Base(entry.absPath), 1<<20)
					files.Close()
					after, statErr := statDiskStamp(entry.absPath)
					if readErr == nil && statErr == nil && before.matches(after) && blake3.Sum256(raw) == prepared.rawHash && pathkey.MutationKey(entry.absPath) == prepared.key {
						exact = true
						reason = publishPhysicalProjection(record, entry, after)
					} else if readErr == nil {
						reason = "readback_changed"
					}
				}
			}
		} else {
			reason = "source_identity_changed"
		}
	}
	record.mu.Lock()
	record.fileReason = reason
	record.readbackExact = exact
	record.mu.Unlock()
}

func (record *mutationCommitRecord) attachFileProjection(response map[string]any) {
	record.mu.RLock()
	sequence, committed := record.commitSeq, record.disk == mutationDiskCommitted
	contentSHA256 := record.contentSHA256
	entry, reason, exact := record.fileProjection.Value(), record.fileReason, record.readbackExact
	record.mu.RUnlock()
	if !committed {
		return
	}
	response["commit_seq"] = sequence
	if contentSHA256 != "" {
		response["content_sha256"] = contentSHA256
	}
	// The original write receipt is historical; a newer registry entry must
	// never be revived or advertised as this receipt's live file projection.
	if liveProjectionForAcknowledgement(entry) {
		response["file_view"] = fileViewForEntry(entry)
		response["readback_exact"] = exact
	} else {
		if reason == "" {
			reason = "projection_not_live"
		}
		response["file_view"] = mutationFileView{State: "unavailable", Reason: reason, Historical: exact, DeltaState: "unavailable", DeltaReason: "projection_not_live", SyntaxHealth: fileViewSyntaxHealth{State: "unknown"}}
		response["readback_exact"] = false
	}
	attachMutationFileTier(response, response["readback_exact"] == true)
	response["graph_status_hint"] = mutationFileGraphHint(response, response["readback_exact"] == true)
	if reason != "" {
		response["file_reason"] = reason
	}
}

// Receipt formatting cannot consume invalidation or perform a hash read.
// Suspect bytes belong to the bounded read-through/lookup revalidation path.
// Check the immutable pointer on both sides of metadata I/O so an old receipt
// cannot claim a concurrently replaced entry, even with identical bytes.
func liveProjectionForAcknowledgement(entry *projectionEntry) bool {
	return projectionLivenessFailure(entry) == projectionOwnerLive
}

// Preserve the original check order and single stat. The returned enum records
// the actual first failed predicate; it is not a later ownership resample.
func projectionLivenessFailure(entry *projectionEntry) projectionLivenessGuard {
	if entry == nil {
		return projectionOwnerMissing
	}
	if entry.suspect.Load() {
		return projectionOwnerSuspectBefore
	}
	if pendingFileProjections.snap.Load().entries[entry.key] != entry {
		return projectionOwnerChangedBefore
	}
	stamp, err := statDiskStamp(entry.absPath)
	if err != nil {
		return projectionOwnerStatError
	}
	if !entry.disk.matches(stamp) {
		return projectionOwnerStampChanged
	}
	if entry.tombstone {
		if stamp.info != nil {
			return projectionOwnerTombstonePresent
		}
	} else if stamp.info == nil || !stamp.info.Mode().IsRegular() {
		return projectionOwnerNotRegular
	}
	if entry.suspect.Load() {
		return projectionOwnerSuspectAfter
	}
	if pendingFileProjections.snap.Load().entries[entry.key] != entry {
		return projectionOwnerChangedAfter
	}
	return projectionOwnerLive
}

// Bind accepted file facts to exactly this physical execution. Tombstones
// carry no syntax verdict because no candidate source bytes were parsed.
func publishPhysicalProjection(record *mutationCommitRecord, entry *projectionEntry, stamp diskStamp) string {
	record.mu.RLock()
	sequence := record.commitSeq
	record.mu.RUnlock()
	entry = entry.committedCopy(sequence, stamp)
	if err := pendingFileProjections.publish(entry); err != nil {
		if refusal, ok := err.(projectionRefusal); ok {
			return string(refusal)
		}
		return "projection_refused"
	}
	record.mu.Lock()
	record.fileProjection = weak.Make(entry)
	record.mu.Unlock()
	if !entry.tombstone {
		if !entry.parseChecked {
			return "syntax_unknown"
		}
		if entry.parseErrors > 0 {
			return "parse_errors"
		}
	}
	return ""
}
