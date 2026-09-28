package graph

import (
	"runtime"
	"strings"
)

// ClaimCount is what the whole-source claims made by one write cost.
type ClaimCount struct {
	Sources int `json:"sources"`
	Edges   int `json:"edges"`
	// Foreign counts the claimed sources outside every covered path: not
	// one of the changed files' own sources.
	Foreign int `json:"foreign"`
}

// claimingFor names the write in progress for the claims it makes, and
// returns the restore. Callers hold writeMu.
func (dw *DeltaWriter) claimingFor(op string) func() {
	prev := dw.claimOp
	dw.claimOp = op
	return func() { dw.claimOp = prev }
}

// noteClaim attributes a whole-source claim to the write in progress and the
// first caller outside this package. Only a claim that claimed a fresh
// source pays for the stack walk. Callers hold writeMu.
func (dw *DeltaWriter) noteClaim(fresh []string, edges int) {
	if len(fresh) == 0 {
		return
	}
	foreign := 0
	for _, id := range fresh {
		if !dw.layer.hasFile(deltaPathKey(id)) {
			foreign++
		}
	}
	key := dw.claimOp
	if key == "" {
		key = "other"
	}
	key += "@" + claimCaller()
	dw.statsMu.Lock()
	if dw.stats.ClaimsByWrite == nil {
		dw.stats.ClaimsByWrite = make(map[string]ClaimCount)
	}
	c := dw.stats.ClaimsByWrite[key]
	c.Sources += len(fresh)
	c.Edges += edges
	c.Foreign += foreign
	dw.stats.ClaimsByWrite[key] = c
	dw.statsMu.Unlock()
}

// claimCaller is the first function on the stack outside the graph package
// (the engine pass that wrote), shortened to package.function.
func claimCaller() string {
	var pcs [24]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		name := f.Function
		if name != "" && !strings.Contains(name, "/internal/graph.") {
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
			return name
		}
		if !more {
			return "unknown"
		}
	}
}
