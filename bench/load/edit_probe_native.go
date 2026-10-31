package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Native senders write the owned fixture outside MCP, as a shell redirection
// or an editor save would. They have no MCP acknowledgement, receipt, phases
// or derived time; each record keeps its own write window instead.
const (
	editProbeShellAfterTool = "shell_after_tool"
	editProbeEditorSave     = "editor_save"
)

type editProbeFileIdentity struct {
	Size    int64   `json:"size"`
	MtimeNS int64   `json:"mtime_ns"`
	Inode   *uint64 `json:"inode"`
}

type editProbeNativeWrite struct {
	Sender            string                 `json:"sender"`
	After             *int                   `json:"after_ordinal,omitempty"`
	DependencyReadyNS *int64                 `json:"dependency_ready_ns,omitempty"`
	StartNS           *int64                 `json:"write_start_ns"`
	EndNS             *int64                 `json:"write_end_ns"`
	Window            Metric                 `json:"write_window_ms"`
	Before            *editProbeFileIdentity `json:"before"`
	Written           *editProbeFileIdentity `json:"after"`
	Replaced          bool                   `json:"replaced"`
}

// editProbeNativeSender admits a native sender only with the fixture shape it
// writes. An empty edit_sender leaves every MCP cell unchanged.
func editProbeNativeSender(c Corpus, kind, tool string, count, editors int) error {
	switch c.EditSender {
	case "":
		return nil
	case editProbeShellAfterTool:
		if kind != "add_intrafile_call" || tool != "edit.file" {
			return errors.New("shell_after_tool pairs edit.file call additions with shell writes of the next marker")
		}
		if count != 100 || editors != 1 {
			return errors.New("shell_after_tool runs 50 tool/shell pairs: 100 edits on one editor")
		}
	case editProbeEditorSave:
		if kind != "rename" || tool != "edit.file" {
			return errors.New("editor_save renames precreated same-length placeholders: it needs the rename fixture through edit.file")
		}
		if editors != 1 {
			return errors.New("editor_save has one editor")
		}
	default:
		return fmt.Errorf("unsupported edit_sender %q", c.EditSender)
	}
	return nil
}

// Odd shell_after_tool ordinals are shell writes that follow the preceding
// tool edit's own ACK; every editor_save ordinal is a native save.
func editProbeNativeStep(sender string, ordinal int) (string, *int, string) {
	switch {
	case sender == editProbeShellAfterTool && ordinal%2 == 1:
		after := ordinal - 1
		return "shell_write", &after, "ack"
	case sender == editProbeEditorSave:
		return "editor_save", nil, ""
	}
	return "", nil, ""
}

// The native request describes the splice the sender writes; it is offered
// load in the plan but never dispatched to MCP.
func editProbeNativeRequest(file, sender string, splice request) request {
	return request{Tool: "native", Operation: sender, Arguments: map[string]any{"path": file, "match": splice.Arguments["match"], "replacement": splice.Arguments["replacement"]}}
}

// A placeholder has exactly the marker's length, so saving the marker over it
// keeps the file size.
func editProbePlaceholder(seed int64, ordinal int) string {
	return fmt.Sprintf("GortexEditProbe%016xP%03d", uint64(seed), ordinal)
}

func editProbeNativeLimitations(sender string) []string {
	out := []string{"native writes have no MCP acknowledgement, receipt, phases or derived time: each record keeps its own write window, its immediate file readback gates t_readback, graph-backed readbacks are observation-only, and it completes once every probe is first-correct inside the deadline", "the daemon learns of a native write only through its watcher, so a fresh claim after the write is counted unassessed, never stale-claimed-fresh"}
	switch sender {
	case editProbeShellAfterTool:
		out = append(out, "odd ordinals are in-place shell writes (truncate, then write) dispatched at their due time or the preceding tool edit's own ACK, whichever is later; a tool edit that overlaps a shell write can lose either change, which is a measured outcome")
	case editProbeEditorSave:
		out = append(out, "every ordinal is an editor save: same-size bytes are written to a temporary file beside the fixture and renamed over it")
	}
	return out
}

func (s *editProbeState) ackSignal() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ackReady == nil {
		s.ackReady = make(chan struct{})
	}
	return s.ackReady
}

func (s *editProbeState) signalAck() {
	ready := s.ackSignal()
	s.ackOnce.Do(func() { close(ready) })
}

func (s *editProbeState) firstCorrectComplete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.record.FirstCorrect {
		if value.Value == nil {
			return false
		}
	}
	return true
}

// A native write runs at its due time, or once the preceding tool edit's own
// ACK is known when it depends on one. It is never retried; a dependency
// without an ACK is an explicit failure and nothing is written.
func (r *editProbeRuntime) doNativeWrite(ctx context.Context, step editProbeStep) {
	native := &editProbeNativeWrite{Sender: step.Sender, After: step.After, Window: unavailable("ms", "native write not performed")}
	var written error
	if step.After != nil {
		predecessor := r.states[*step.After]
		select {
		case <-predecessor.ackSignal():
		case <-ctx.Done():
		}
		predecessor.mu.Lock()
		acknowledged := !predecessor.acknowledged.IsZero()
		predecessor.mu.Unlock()
		ready := time.Since(r.origin).Nanoseconds()
		native.DependencyReadyNS = &ready
		if !acknowledged && ctx.Err() == nil {
			written = fmt.Errorf("native write dependency failed: ordinal %d has no own acknowledgement", *step.After)
		}
	}
	r.editsInFlight.Add(1)
	defer r.editsInFlight.Add(-1)
	s, callCtx, cancel, started := r.beginEdit(ctx, step)
	defer s.stopProbes()
	defer cancel()
	if written == nil && ctx.Err() == nil {
		written = r.writeNative(s, step, native)
	}
	s.mu.Lock()
	s.record.NativeWrite = native
	s.record.Receipt, s.record.MutationID = "", ""
	s.record.Ack = unavailable("ms", "native write has no MCP acknowledgement")
	s.record.Accounting = editProbeAccounting{State: "not_applicable_native_write"}
	s.record.PhasesMS = editProbePhases(nil, "native write has no mutation receipt")
	s.record.Derived = unavailable("ms", "native write has no mutation receipt")
	s.record.Readback = unavailable("ms", "native write or its immediate file readback failed")
	if written != nil {
		s.record.Errors = append(s.record.Errors, written.Error())
	}
	s.mu.Unlock()
	if written == nil {
		kinds := []string{"file", "source"}
		if step.BeforeSymbol != "" {
			kinds = append(kinds, "old_source_absent")
		}
		// The file is on disk once the write returns; graph answers may lag
		// the watcher, so they are recorded without gating anything.
		end, correct := r.immediateReadbacks(callCtx, s, step, kinds, map[string]bool{"source": true, "old_source_absent": true})
		if correct {
			s.mu.Lock()
			s.record.Readback = measured(float64(end.Sub(started))/float64(time.Millisecond), "ms")
			s.mu.Unlock()
		}
		for callCtx.Err() == nil && !s.firstCorrectComplete() {
			if waitContext(callCtx, editProbePeriod) != nil {
				break
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if written == nil && (s.firstProbeWrite.IsZero() || s.editWrite.IsZero() || !s.firstProbeWrite.Before(s.editWrite)) {
		s.record.Errors = append(s.record.Errors, "no actual prober wire start before native write")
	}
	for name, value := range s.record.FirstCorrect {
		if value.Value == nil {
			s.record.Errors = append(s.record.Errors, "first-correct oracle unavailable: "+name)
		}
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

// writeNative writes only the registered owned fixture. A shell write
// truncates and rewrites the file in place; an editor save writes same-size
// bytes to a temporary file beside it and renames that over the fixture.
func (r *editProbeRuntime) writeNative(s *editProbeState, step editProbeStep, native *editProbeNativeWrite) error {
	path, _ := step.Request.Arguments["path"].(string)
	match, _ := step.Request.Arguments["match"].(string)
	replacement, _ := step.Request.Arguments["replacement"].(string)
	repo := r.p.Repos[0].Path
	target := filepath.Join(repo, filepath.FromSlash(path))
	if step.Request.Tool != "native" || step.Request.Operation != step.Sender || path != r.plan.File || match == "" || !inside(repo, target) {
		return errors.New("native sender writes only the registered owned fixture file")
	}
	before, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return errors.New("native sender writes only a regular owned fixture file")
	}
	native.Before = editProbeIdentity(before)
	current, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if strings.Count(string(current), match) != 1 {
		return errors.New("native write splice does not occur exactly once in the current fixture bytes")
	}
	next := []byte(strings.Replace(string(current), match, replacement, 1))
	if step.Sender == "editor_save" && len(next) != len(current) {
		return errors.New("editor save must keep the fixture size")
	}
	start := time.Now()
	s.mu.Lock()
	s.editWrite = start
	s.mu.Unlock()
	if step.Sender == "editor_save" {
		err = editProbeSaveReplace(target, step.Ordinal, next, before.Mode().Perm())
	} else {
		err = editProbeShellWrite(target, next)
	}
	end := time.Now()
	startNS, endNS := start.Sub(r.origin).Nanoseconds(), end.Sub(r.origin).Nanoseconds()
	native.StartNS, native.EndNS = &startNS, &endNS
	native.Window = measured(float64(end.Sub(start))/float64(time.Millisecond), "ms")
	if after, statErr := os.Lstat(target); statErr == nil {
		native.Written, native.Replaced = editProbeIdentity(after), !os.SameFile(before, after)
	}
	row := map[string]any{"event": "native_write", "sender": step.Sender, "path": path, "write_start_ns": startNS, "write_end_ns": endNS, "before": native.Before, "after": native.Written, "replaced": native.Replaced}
	if err != nil {
		row["error"] = err.Error()
	}
	return errors.Join(err, s.emit(row))
}

// Shell redirection semantics: the existing file is truncated, then written.
func editProbeShellWrite(target string, content []byte) error {
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = file.Write(content)
	return errors.Join(err, file.Close())
}

// Editor save semantics: a temporary file with no source extension is written
// beside the fixture, then renamed over it.
func editProbeSaveReplace(target string, ordinal int, content []byte, mode os.FileMode) error {
	temp := filepath.Join(filepath.Dir(target), fmt.Sprintf(".%s.%03d.gortex-save.tmp", filepath.Base(target), ordinal))
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = file.Write(content)
	err = errors.Join(err, file.Close())
	if err == nil {
		err = os.Rename(temp, target)
	}
	if err != nil {
		_ = os.Remove(temp)
	}
	return err
}

func editProbeIdentity(info os.FileInfo) *editProbeFileIdentity {
	return &editProbeFileIdentity{Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), Inode: editProbeInode(info)}
}
