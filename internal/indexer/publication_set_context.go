package indexer

import (
	"context"
	"path/filepath"
)

// filePublicationRecordsKey is an admission-only carrier, not a registry.
// It never changes record ownership or creates another publication timeline.
type filePublicationRecordsKey struct{}

// WithFilePublicationRecords carries the original per-file records through a
// single set admission. Copying the map prevents callers from rebinding a
// member during admission. Ambiguous path/record aliases remain unavailable.
func WithFilePublicationRecords(ctx context.Context, records map[string]*PublicationPhaseRecord) context.Context {
	if ctx == nil {
		if len(records) == 0 {
			return nil
		}
		ctx = context.Background()
	}
	_, priorSet := ctx.Value(filePublicationRecordsKey{}).(map[string]*PublicationPhaseRecord)
	if len(records) == 0 && !priorSet && publicationRecordFrom(ctx) == nil {
		return ctx // truly unobserved admission remains allocation-free
	}
	copied := make(map[string]*PublicationPhaseRecord, len(records))
	ambiguous := make(map[string]bool)
	uses := make(map[*PublicationPhaseRecord]int)
	for path, record := range records {
		if record == nil || !filepath.IsAbs(path) {
			continue
		}
		path = filepath.Clean(path)
		uses[record]++ // count all raw aliases, including an ambiguous path
		if _, duplicate := copied[path]; duplicate || ambiguous[path] {
			delete(copied, path)
			ambiguous[path] = true
			continue
		}
		copied[path] = record
	}
	for path, record := range copied {
		if uses[record] != 1 {
			delete(copied, path)
		}
	}
	return context.WithValue(ctx, filePublicationRecordsKey{}, copied)
}

// A present carrier shadows the single-record context even for an unknown or
// refused member. One file's record can never stand in for another file.
func filePublicationRecordFrom(ctx context.Context, path string) *PublicationPhaseRecord {
	if ctx == nil {
		return nil
	}
	if records, present := ctx.Value(filePublicationRecordsKey{}).(map[string]*PublicationPhaseRecord); present {
		return records[filepath.Clean(path)]
	}
	return publicationRecordFrom(ctx)
}

func hasFilePublicationRecords(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if records, present := ctx.Value(filePublicationRecordsKey{}).(map[string]*PublicationPhaseRecord); present {
		return len(records) != 0
	}
	return publicationRecordFrom(ctx) != nil
}
