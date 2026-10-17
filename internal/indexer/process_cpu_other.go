//go:build !unix

package indexer

import "time"

// processCPUTime is unavailable here.
func processCPUTime() time.Duration { return 0 }
