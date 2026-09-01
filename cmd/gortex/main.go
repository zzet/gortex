package main

import "github.com/zzet/gortex/internal/progress"

// Build-time variables injected via `-X` ldflags. goreleaser populates
// them from git state; `make build` does the same via Makefile variables.
// When built with plain `go build`, all three stay at their defaults and
// `gortex version` prints a clear "(dev build)" notice.
//
// Order matters for goreleaser ldflag templates — keep these unchanged.
var (
	version = "0.63.9" // SemVer 2.0.0 string without build slot (e.g. "0.1.0", "0.1.0-rc1")
	commit  = ""       // short git SHA (e.g. "abc1234"); becomes +<build> in the canonical form
	date    = ""       // RFC-3339 build timestamp
)

func main() {
	// A panic that unwinds out of a command must not leave the terminal with
	// a hidden cursor from a live progress animation.
	defer progress.RestoreTerminal()
	execute()
}
