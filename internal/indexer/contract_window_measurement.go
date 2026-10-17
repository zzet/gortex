package indexer

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

var contractWindowProcess = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
var contractWindowJobs atomic.Uint64

func contractWindowUnixNano(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixNano()
}
