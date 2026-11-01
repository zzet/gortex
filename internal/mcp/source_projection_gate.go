package mcp

import (
	"context"
	"encoding/hex"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer"
)

// prepareAcknowledgedProjection builds only caller-owned file facts while the
// original source path is still held. A refused projection preserves the
// existing syntax gate; it cannot relax write admission.
func (s *Server) prepareAcknowledgedProjection(ctx context.Context, path, absPath string, before, after []byte) (*projectionEntry, parseGateResult) {
	s.noteAcknowledgementParser(ctx, "source_projection_parse_gate")
	lap := s.beginAcknowledgementLap(ctx, editAckParseGate)
	defer lap.finish()
	var entry *projectionEntry
	var prepared *indexer.Projection
	ownerPath := s.overlayOwnerAbsPath(ctx, absPath)
	if idx := s.pickIndexerForPath(ownerPath); idx != nil {
		projection, err := idx.ProjectSource(ctx, ownerPath, after)
		if err == nil {
			prepared = projection
			entry = newProjectionEntry(absPath, idx.RepoPrefix(), projection, after, 0, diskStamp{})
		}
	}
	gate := projectedParseGate(path, before, after, prepared)
	lap.applicable = gate.Checked
	return entry, gate
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
