package main

import (
	"testing"
	"time"
)

func TestEditProbeConcurrentCommitWitness(t *testing.T) {
	origin := time.Unix(1000, 0)
	at := func(ms int) time.Time { return origin.Add(time.Duration(ms) * time.Millisecond) }
	sent, received := at(100), at(200)
	candidates := []editProbeCommitCandidate{
		{ordinal: 1, editor: 1, target: "repo\x00a.go", editWrite: at(20), acknowledged: at(150), commitSeq: 9, receipt: "r1"}, // ACK lands inside the read
		{ordinal: 2, editor: 2, target: "repo\x00a.go", editWrite: at(190), acknowledged: at(400), commitSeq: 10},              // in flight at receive
		{ordinal: 3, editor: 3, target: "repo\x00a.go", editWrite: at(10), acknowledged: at(90), commitSeq: 8},                 // acknowledged before send
		{ordinal: 4, editor: 4, target: "repo\x00b.go", editWrite: at(110), acknowledged: at(120), commitSeq: 11},              // another file
		{ordinal: 5, editor: 0, target: "repo\x00a.go", editWrite: at(50), acknowledged: at(160), commitSeq: 12},               // the reading edit itself
		{ordinal: 6, editor: 6, target: "repo\x00a.go", editWrite: at(250), acknowledged: at(300), commitSeq: 13},              // sent after receive
		{ordinal: 7, editor: 7, target: "repo\x00a.go", editWrite: at(30)},                                                     // never acknowledged
	}
	got := editProbeCommitWitnessFor(5, "repo\x00a.go", sent, received, origin, candidates)
	if got.State != "observed" || len(got.Commits) != 3 {
		t.Fatalf("witness = %+v", got)
	}
	first, second, third := got.Commits[0], got.Commits[1], got.Commits[2]
	if first.Ordinal != 1 || !first.ACKInWindow || first.CommitSeq != 9 || first.Receipt != "r1" || first.ACKNS == nil || *first.ACKNS != int64(150*time.Millisecond) {
		t.Fatalf("ACK inside the read window not witnessed: %+v", first)
	}
	if second.Ordinal != 2 || second.ACKInWindow || second.EditWriteNS == nil || *second.EditWriteNS != int64(190*time.Millisecond) {
		t.Fatalf("in-flight edit not witnessed apart from an in-window ACK: %+v", second)
	}
	if third.Ordinal != 7 || third.ACKInWindow || third.ACKNS != nil {
		t.Fatalf("unacknowledged overlapping edit invented an ACK: %+v", third)
	}
	if none := editProbeCommitWitnessFor(5, "repo\x00b.go", at(130), at(140), origin, candidates); none.State != "none_observed" || len(none.Commits) != 0 {
		t.Fatalf("no overlap became a witness: %+v", none)
	}
	for _, window := range [][2]time.Time{{{}, received}, {sent, {}}, {received, sent}} {
		if got := editProbeCommitWitnessFor(5, "repo\x00a.go", window[0], window[1], origin, candidates); got.State != "unavailable" || len(got.Commits) != 0 {
			t.Fatalf("missing read window became a witness: %+v", got)
		}
	}
}
