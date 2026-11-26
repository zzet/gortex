package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Once an oracle's first-correct time is recorded and the receipt reports the
// graph terminal, a background probe whose one successful attempt returns
// exactly the bytes of that oracle's previous correct answer repeats evidence
// the spool already holds. Its rpc, logical_call and probe rows are folded
// into one bounded repeat_fold row per run of identical answers. The record,
// attempts, output-mode counts and stale checks still observe every call;
// only the per-edit JSONL is compacted. Immediate readbacks, the edit ACK,
// receipt status, CLI probes and relation consumer probes are never folded,
// and a run ends at the first answer that differs, fails, is pending or is
// not correct. That answer is kept in full.

type editProbeHeldRowsKey struct{}

// One probe iteration's rpc and logical_call rows, held until that
// iteration's verdict decides whether they fold.
type editProbeHeldRows struct {
	purpose string
	base    []byte
	rows    []map[string]any
}

type editProbeRepeatRun struct {
	count                   int
	digest                  string
	bytes                   int
	outputMode              any
	claimsFresh             any
	firstWireID, lastWireID any
	firstStartNS, lastEndNS any
}

type editProbeRepeats struct {
	base   map[string][]byte // purpose -> the oracle's last correct answer
	runs   map[string]*editProbeRepeatRun
	folded int
}

func editProbeHeldRowsFrom(ctx context.Context, purpose string) *editProbeHeldRows {
	held, _ := ctx.Value(editProbeHeldRowsKey{}).(*editProbeHeldRows)
	if held == nil || held.purpose != purpose {
		return nil
	}
	return held
}

// A held iteration keeps its rows until settleRepeat; every other call emits.
func (s *editProbeState) emitOrHold(ctx context.Context, purpose string, row map[string]any) error {
	if held := editProbeHeldRowsFrom(ctx, purpose); held != nil {
		held.rows = append(held.rows, row)
		return nil
	}
	return s.emit(row)
}

// repeatHold starts one background probe iteration. A settled oracle with a
// previous correct answer holds the iteration's rows; otherwise its open run
// is written first so the spool keeps call order.
func (s *editProbeState) repeatHold(purpose string) (*editProbeHeldRows, error) {
	s.mu.Lock()
	settled := s.record.GraphTerminal && s.record.FirstCorrect[strings.TrimPrefix(purpose, "oracle:")].Value != nil
	s.mu.Unlock()
	s.repeatMu.Lock()
	base := s.repeats.base[purpose]
	s.repeatMu.Unlock()
	if settled && base != nil {
		return &editProbeHeldRows{purpose: purpose, base: base}, nil
	}
	return nil, s.flushRepeat(purpose)
}

// settleRepeat folds a held iteration that repeated the oracle's previous
// correct answer, or writes the open run, the held rows and the probe row.
func (s *editProbeState) settleRepeat(held *editProbeHeldRows, purpose string, data json.RawMessage, callErr error, row map[string]any) error {
	correct := callErr == nil && row["correct"] == true && row["receipt_observed_pending"] == false
	if held != nil && correct && s.foldRepeat(held, purpose, data, row) {
		return nil
	}
	err := s.flushRepeat(purpose)
	if held != nil {
		for _, heldRow := range held.rows {
			err = errors.Join(err, s.emit(heldRow))
		}
	}
	err = errors.Join(err, s.emit(row))
	s.repeatMu.Lock()
	defer s.repeatMu.Unlock()
	switch {
	case !correct:
		delete(s.repeats.base, purpose) // The next correct answer is kept in full.
	case !bytes.Equal(s.repeats.base[purpose], data):
		if s.repeats.base == nil {
			s.repeats.base = make(map[string][]byte)
		}
		s.repeats.base[purpose] = bytes.Clone(data)
	}
	return err
}

func (s *editProbeState) foldRepeat(held *editProbeHeldRows, purpose string, data json.RawMessage, row map[string]any) bool {
	if len(held.rows) != 2 {
		return false
	}
	rpc, logical := held.rows[0], held.rows[1]
	payload, _ := rpc["payload"].(json.RawMessage)
	_, failed := rpc["error"]
	attempts, _ := logical["attempts"].([]Attempt)
	if rpc["event"] != "rpc" || logical["event"] != "logical_call" || failed || len(attempts) != 1 || attempts[0].Outcome != "success" ||
		!bytes.Equal(payload, held.base) || !bytes.Equal(data, held.base) {
		return false
	}
	s.mu.Lock()
	settled := s.record.GraphTerminal
	s.mu.Unlock()
	if !settled {
		return false
	}
	s.repeatMu.Lock()
	defer s.repeatMu.Unlock()
	run := s.repeats.runs[purpose]
	if run != nil && (run.outputMode != row["output_mode"] || run.claimsFresh != row["claims_fresh"]) {
		return false
	}
	if run == nil {
		run = &editProbeRepeatRun{digest: fmt.Sprintf("%x", sha256.Sum256(data)), bytes: len(data), outputMode: row["output_mode"], claimsFresh: row["claims_fresh"], firstWireID: rpc["wire_id"], firstStartNS: row["start_ns"]}
		if s.repeats.runs == nil {
			s.repeats.runs = make(map[string]*editProbeRepeatRun)
		}
		s.repeats.runs[purpose] = run
	}
	run.count++
	run.lastWireID, run.lastEndNS = rpc["wire_id"], row["end_ns"]
	s.repeats.folded++
	return true
}

func (s *editProbeState) flushRepeat(purpose string) error {
	s.repeatMu.Lock()
	run := s.repeats.runs[purpose]
	delete(s.repeats.runs, purpose)
	s.repeatMu.Unlock()
	if run == nil {
		return nil
	}
	return s.emit(map[string]any{"event": "repeat_fold", "purpose": purpose, "oracle": strings.TrimPrefix(purpose, "oracle:"), "count": run.count,
		"payload_sha256": run.digest, "payload_bytes": run.bytes, "output_mode": run.outputMode, "correct": true, "claims_fresh": run.claimsFresh,
		"first_wire_id": run.firstWireID, "last_wire_id": run.lastWireID, "first_start_ns": run.firstStartNS, "last_end_ns": run.lastEndNS})
}

// Every open run, after the probes have stopped and before the writer closes.
func (s *editProbeState) flushRepeats() error {
	s.repeatMu.Lock()
	purposes := slices.Sorted(maps.Keys(s.repeats.runs))
	s.repeatMu.Unlock()
	var err error
	for _, purpose := range purposes {
		err = errors.Join(err, s.flushRepeat(purpose))
	}
	return err
}

func (s *editProbeState) foldedRepeats() int {
	s.repeatMu.Lock()
	defer s.repeatMu.Unlock()
	return s.repeats.folded
}
