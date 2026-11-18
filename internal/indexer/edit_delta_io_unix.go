//go:build unix

package indexer

import "syscall"

// editDeltaIO is a snapshot of the process's page-in counters.
type editDeltaIO struct{ majorFaults, blockReads int64 }

func editDeltaProcessIO() editDeltaIO {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return editDeltaIO{}
	}
	return editDeltaIO{majorFaults: int64(usage.Majflt), blockReads: int64(usage.Inblock)}
}

func (io editDeltaIO) since(start editDeltaIO) editDeltaIO {
	return editDeltaIO{majorFaults: io.majorFaults - start.majorFaults, blockReads: io.blockReads - start.blockReads}
}
