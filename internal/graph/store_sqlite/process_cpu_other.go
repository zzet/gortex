//go:build !unix

package store_sqlite

import "time"

// processCPUTime is unavailable here.
func storeProcessCPUTime() time.Duration { return 0 }
