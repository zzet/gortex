package mcp

import (
	"context"
	"os"
)

// These fixed caller boundaries surround only forward source durability.
// Journal/backup/rollback work remains separate and unclassified; no parent
// batch interval encloses these chunks or adds them a second time.
func (s *Server) writeAcknowledgedBatchFile(ctx context.Context, writer func(string, []byte, os.FileMode) error, path string, content []byte, mode os.FileMode) error {
	lap := s.beginAcknowledgementLap(ctx, editAckDiskWrite)
	defer lap.finish()
	return writer(path, content, mode)
}

func (s *Server) removeAcknowledgedBatchFile(ctx context.Context, remover func(string) error, path string) error {
	lap := s.beginAcknowledgementLap(ctx, editAckDiskWrite)
	defer lap.finish()
	return remover(path)
}

func (s *Server) syncAcknowledgedBatchDirectories(ctx context.Context, dirs ...string) error {
	lap := s.beginAcknowledgementLap(ctx, editAckDiskWrite)
	defer lap.finish()
	return s.syncBatchDirectories(dirs...)
}
