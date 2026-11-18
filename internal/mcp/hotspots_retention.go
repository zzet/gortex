package mcp

import (
	"strings"
	"unsafe"

	"github.com/zzet/gortex/internal/analysis"
)

const hotspotResultRetentionBytes = 1 << 20

// hotspotResultRetainable bounds the complete result's owned compact payload.
// Eligibility alone does not bound aliases: retention must copy the slice and
// clone its strings, since a tiny substring can keep a large allocation alive.
func hotspotResultRetainable(entries []analysis.HotspotEntry) bool {
	entryBytes := int(unsafe.Sizeof(analysis.HotspotEntry{}))
	remaining := hotspotResultRetentionBytes - int(unsafe.Sizeof(entries))
	if len(entries) > remaining/entryBytes {
		return false
	}
	remaining -= len(entries) * entryBytes
	for _, entry := range entries {
		for _, value := range []string{entry.ID, entry.Name, entry.Kind, entry.FilePath} {
			if len(value) > remaining {
				return false
			}
			remaining -= len(value)
		}
	}
	return true
}

func compactHotspotResult(entries []analysis.HotspotEntry) []analysis.HotspotEntry {
	if entries == nil {
		return nil
	}
	owned := make([]analysis.HotspotEntry, len(entries))
	for i, entry := range entries {
		entry.ID = strings.Clone(entry.ID)
		entry.Name = strings.Clone(entry.Name)
		entry.Kind = strings.Clone(entry.Kind)
		entry.FilePath = strings.Clone(entry.FilePath)
		owned[i] = entry
	}
	return owned
}
