package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/zzet/gortex/internal/pathkey"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer"
)

// prepareAcknowledgedProjection builds only caller-owned file facts while the
// original source path is still held. A refused projection preserves the
// existing syntax gate; it cannot relax write admission.
func (s *Server) prepareAcknowledgedProjection(ctx context.Context, path, absPath string, before, after []byte) (*preparedFileProjection, parseGateResult) {
	s.noteAcknowledgementParser(ctx, "source_projection_parse_gate")
	lap := s.beginAcknowledgementLap(ctx, editAckParseGate)
	defer lap.finish()
	prepared, syntax := s.buildFileProjection(ctx, absPath, after)
	gate := projectedParseGate(path, before, after, syntax)
	lap.applicable = gate.Checked
	lap.finish()
	s.prepareFileViewEvidence(ctx, prepared, baselineForPreparedWrite(absPath, before), after)
	return prepared, gate
}

// Coordinate preservation does not prove equal syntax: a prepass can blank
// bytes without moving them. Reuse only a checked full parse of identical
// candidate bytes and the very same gate language, never a default zero.
func projectedParseGate(path string, before, after []byte, projection *indexer.Projection) parseGateResult {
	if projection != nil && projection.ParseChecked && projection.ParseErrors >= 0 && projection.Language == parseGateLanguage(path) {
		sum := blake3.Sum256(after)
		if projection.ContentHash == hex.EncodeToString(sum[:]) {
			return checkParseGateWithVerdict(path, before, after, true, projection.ParseErrors)
		}
	}
	return checkParseGate(path, before, after)
}

// This short-lived preparation never becomes a second authority or a ledger
// graph cache. The registry alone may retain its admitted entry after commit.
type preparedFileProjection struct {
	key           string
	rawHash       [32]byte
	contentSHA256 string
	entry         *projectionEntry
	expected      *projectionEntry
	refusal       string
}

func (s *Server) buildFileProjection(ctx context.Context, absPath string, raw []byte) (*preparedFileProjection, *indexer.Projection) {
	key := pathkey.MutationKey(absPath)
	sum := sha256.Sum256(raw)
	prepared := &preparedFileProjection{key: key, rawHash: blake3.Sum256(raw), contentSHA256: hex.EncodeToString(sum[:]), expected: pendingFileProjections.snap.Load().entries[key]}
	if !pendingFileProjections.enabled {
		prepared.refusal = "disabled"
		return prepared, nil
	}
	if ctx.Err() != nil {
		prepared.refusal = "cancelled"
		return prepared, nil
	}
	ownerPath := s.overlayOwnerAbsPath(ctx, absPath)
	idx := s.pickIndexerForPath(ownerPath)
	if idx == nil {
		prepared.refusal = "parser_unavailable"
		return prepared, nil
	}
	projection, err := idx.ProjectSource(ctx, ownerPath, raw)
	if err != nil {
		switch {
		case errors.Is(err, indexer.ErrProjectionTooLarge):
			prepared.refusal = "source_too_large"
		case errors.Is(err, indexer.ErrProjectionLanguage):
			prepared.refusal = "language_not_projected"
		case errors.Is(err, indexer.ErrProjectionTransform):
			prepared.refusal = "transform_not_projected"
		case errors.Is(err, indexer.ErrProjectionNoSlot):
			prepared.refusal = "parse_slot_deadline"
		case errors.Is(err, indexer.ErrProjectionDeadline):
			prepared.refusal = "build_deadline"
		default:
			prepared.refusal = "extraction_refused"
		}
		return prepared, nil
	}
	prepared.entry = newProjectionEntry(absPath, idx.RepoPrefix(), projection, raw, 0, diskStamp{})
	prepared.entry.contentSHA256 = prepared.contentSHA256
	prepared.entry.size += 128 // separately owned raw SHA256 and allocation/header allowance
	return prepared, projection
}

// Removal owns the directory entry, including a leaf symlink; it never parses
// or follows the removed leaf to construct the file tombstone.
func (s *Server) buildRemovalProjection(ctx context.Context, relPath, absPath string) *preparedFileProjection {
	key := pathkey.MutationKey(absPath)
	prepared := &preparedFileProjection{key: key, expected: pendingFileProjections.snap.Load().entries[key]}
	if !pendingFileProjections.enabled {
		prepared.refusal = "disabled"
		return prepared
	}
	if ctx.Err() != nil {
		prepared.refusal = "cancelled"
		return prepared
	}
	idx := s.pickIndexerForPath(s.overlayOwnerAbsPath(ctx, absPath))
	if idx == nil {
		prepared.refusal = "parser_unavailable"
		return prepared
	}
	graphPath := s.resolveOverlayGraphPathForRequest(ctx, relPath, absPath)
	prepared.entry = newProjectionTombstone(absPath, graphPath, idx.RepoPrefix(), 0)
	return prepared
}
