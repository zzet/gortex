//go:build !unix

package indexer

// editDeltaIO is a snapshot of the process's page-in counters; none are
// available off unix.
type editDeltaIO struct{ majorFaults, blockReads int64 }

func editDeltaProcessIO() editDeltaIO { return editDeltaIO{} }

func (io editDeltaIO) since(editDeltaIO) editDeltaIO { return editDeltaIO{} }
