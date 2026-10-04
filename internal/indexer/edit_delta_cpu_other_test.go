//go:build !unix

package indexer

import "time"

// editDeltaProcessCPU is unavailable off unix; the harness reports 0.
func editDeltaProcessCPU() time.Duration { return 0 }
