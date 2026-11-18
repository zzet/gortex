package mcp

import (
	"sync/atomic"
	"time"
)

func epochMutationSequence() *atomic.Uint64 {
	sequence := new(atomic.Uint64)
	sequence.Store(uint64(time.Now().UnixNano()))
	return sequence
}

func lostMutationPayload(key, id string) map[string]any {
	return map[string]any{
		key: id, "found": false, "state": "lost", "graph_status_terminal": true,
		"note": "this process has no retained receipt; read the current file or request a scoped refresh; a lost receipt cannot certify freshness",
	}
}
