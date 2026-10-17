package store_sqlite

import "sync/atomic"

// writeTransactionsBegun counts the write transactions every store of the
// process has begun (beginWriteOnContext). A build samples it before and
// after a step to report how many transactions the step committed; the count
// is process-wide, so a concurrent writer's transactions land in the same
// difference, as a WAL mark's frames do.
var writeTransactionsBegun atomic.Int64

// WriteTransactionsBegun is the number of write transactions begun in this
// process so far.
func WriteTransactionsBegun() int64 { return writeTransactionsBegun.Load() }
