package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// TypeScript and Python relocation is offered as the actual move, which the
// product refuses; Go relocation is supported and measured as a move. The
// refusal cell is explicit (edit_expect: "refusal"), never inferred.
func editProbeRelocationRefusalFixture(c Corpus) error {
	ext := filepath.Ext(c.File)
	if c.EditExpect != "refusal" || c.EditKind != "relocate" || ext != ".ts" && ext != ".py" {
		return errors.New("unsupported language: move_symbol fixture supports Go only; TypeScript/Python relocation is an explicit edit_expect refusal cell")
	}
	if filepath.Base(c.File) != c.File || filepath.Ext(c.EditDestination) != ext || filepath.Base(c.EditDestination) != c.EditDestination || c.File == c.EditDestination || !utf8.ValidString(c.EditDestinationContent) {
		return errors.New("relocation refusal fixture requires distinct root source and destination files of one language")
	}
	return editProbeInitialFixture(c)
}

// The refusal must be the producer's one scoped message as the only payload,
// followed at most by the known auxiliary session notes.
func editProbeExactToolRefusal(raw json.RawMessage, want string) (string, bool) {
	var envelope struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Content) == 0 {
		return "", false
	}
	message := envelope.Content[0].Text
	lastNoteKind := 0
	for _, item := range envelope.Content[1:] {
		kind := editProbeSessionNoteKind(item.Text)
		if item.Type != "text" || kind <= lastNoteKind {
			return message, false
		}
		lastNoteKind = kind
	}
	return message, envelope.IsError && envelope.Content[0].Type == "text" && message == want
}

// A refusal cell completes only on the exact scoped refusal, then the same
// editing session's immediate file readback and the original symbol's source
// show no change, and the source and destination bytes on disk are unchanged.
// Any acknowledgement, receipt or other message is a diagnostic failure.
func (r *editProbeRuntime) finishRefusal(ctx, callCtx context.Context, s *editProbeState, step editProbeStep, started time.Time, raw json.RawMessage, identity wireCallIdentity, err error) {
	want := fmt.Sprintf("unsupported language: move_symbol only supports Go files (got %q)", r.p.Repos[0].Prefix+"/"+r.plan.File)
	refusal := editProbeReadback{Kind: "relocation_refusal", Latency: unavailable("ms", "response endpoint unavailable"), ExpectedOutcome: "refused"}
	if !identity.Finished.IsZero() {
		refusal.Latency = measured(float64(identity.Finished.Sub(started))/float64(time.Millisecond), "ms")
	}
	message, exact := editProbeExactToolRefusal(raw, want)
	refusal.Refusal, refusal.Correct = message, err != nil && exact
	if !refusal.Correct && err != nil {
		refusal.Error = err.Error()
	}
	s.mu.Lock()
	s.record.Receipt, s.record.MutationID = "", ""
	s.record.Ack = unavailable("ms", "relocation refused; its latency is the relocation_refusal readback")
	s.record.Accounting = editProbeAccounting{State: "not_applicable_refused"}
	s.record.PhasesMS = editProbePhases(nil, "refusal cell made no change")
	s.record.Derived = unavailable("ms", "refusal cell made no change")
	s.record.Readback = unavailable("ms", "refusal readbacks missing or incorrect")
	s.record.Readbacks = append(s.record.Readbacks, refusal)
	if !refusal.Correct {
		s.record.Errors = append(s.record.Errors, "relocation was not refused with the exact producer message")
	}
	s.mu.Unlock()
	readbacksCorrect, readbackEnd := refusal.Correct, identity.Finished
	for index, req := range []request{
		editProbeWithOutput(request{Tool: "read", Operation: "file", Arguments: map[string]any{"operation": "file", "target": map[string]any{"file": r.plan.File}}}, r.plan.Mode),
		{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": step.BeforeSymbol}, "options": map[string]any{"context_lines": 0}}},
	} {
		begin := time.Now()
		data, readIdentity, readErr := r.perform(callCtx, s, r.editorFor(step), req, "readback:"+req.Operation, "readback", 100+index)
		back := editProbeReadback{Kind: "file_unchanged", Latency: unavailable("ms", "response endpoint unavailable")}
		if req.Operation == "source" {
			back.Kind, back.Correct = "original_source_present", readErr == nil && editProbePresenceProof(data, r.p.Repos[0].Corpus, step.BeforeSymbol)
		} else {
			body, decodeErr := decodeEditProbeOutput(data, editProbeRequestOutput(req), req.name())
			content, _ := body["content"].(string)
			path, _ := body["path"].(string)
			back.Correct = readErr == nil && decodeErr == nil && content == string(r.original) && editProbeFixturePath(path, r.plan.File)
			if decodeErr != nil {
				back.Error = decodeErr.Error()
			}
		}
		if readErr != nil {
			back.Error = readErr.Error()
		}
		if !readIdentity.Finished.IsZero() {
			back.Latency = measured(float64(readIdentity.Finished.Sub(begin))/float64(time.Millisecond), "ms")
			readbackEnd = readIdentity.Finished
		}
		readbacksCorrect = readbacksCorrect && back.Correct && !readIdentity.Finished.IsZero()
		s.mu.Lock()
		s.record.Readbacks = append(s.record.Readbacks, back)
		if !back.Correct {
			s.record.Errors = append(s.record.Errors, "immediate "+back.Kind+" readback incorrect")
		}
		s.mu.Unlock()
	}
	unchanged := r.confirmRefusalUnchanged()
	s.mu.Lock()
	defer s.mu.Unlock()
	if readbacksCorrect {
		s.record.Readback = measured(float64(readbackEnd.Sub(started))/float64(time.Millisecond), "ms")
	}
	if unchanged != nil {
		s.record.Errors = append(s.record.Errors, unchanged.Error())
	}
	switch {
	case ctx.Err() != nil:
		s.record.Outcome = "phase_censored"
	case callCtx.Err() != nil:
		s.record.Outcome = "product_deadline"
	case len(s.record.Errors) == 0:
		s.record.Outcome = "complete"
	default:
		s.record.Outcome = "diagnostic_failure"
	}
}

// A refusal cell has nothing to restore; its post-window step only proves
// that the source and destination bytes are still the offered originals.
func (r *editProbeRuntime) confirmRefusalUnchanged() error {
	if r.phase != nil && r.phase.EditProbe != nil {
		r.phase.EditProbe.RestoreMS = unavailable("ms", "refusal cell made no change")
	}
	source, err := os.ReadFile(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(r.plan.File)))
	if err != nil {
		return err
	}
	destination, err := os.ReadFile(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(r.p.Repos[0].Corpus.EditDestination)))
	if err != nil {
		return err
	}
	if string(source) != string(r.original) || string(destination) != string(r.moveOriginal) {
		return errors.New("refused relocation changed source or destination bytes")
	}
	return nil
}
