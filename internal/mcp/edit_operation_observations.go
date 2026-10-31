package mcp

import (
	"bytes"
	"context"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// These are Server-wide actual-handler outcomes, not delivered acknowledgements,
// per-file commits, selected-view counts, or native editor operations. A daemon's
// SharedServer shares them across its clients. No process sampling occurs here.
// All fields are monotonic for this Server's lifetime; reading never resets them.
type editOperationCounters struct {
	completed               atomic.Uint64
	committedWithError      atomic.Uint64
	failedAfterSourceEffect atomic.Uint64
	rolledBack              atomic.Uint64 // subset of failedAfterSourceEffect
	unknownSourceComparison atomic.Uint64
}

type editOperationContextKey struct{}

type editOperationWitness struct {
	owner *editOperationCounters
	flags atomic.Uint32
}

const (
	editSourceEffect uint32 = 1 << iota
	editSourceComplete
	editSourceWarning
	editSourceRolledBack
	editSourceUnknownComparison
)

// measuredSourceEdit deliberately follows the five requested edit surfaces,
// including their facade spellings. Other tools and unresolved operations never
// allocate a witness. Physical watcher patches are not logical editor stimuli.
func measuredSourceEdit(req mcp.CallToolRequest) bool {
	switch req.Params.Name {
	case "write_file", "edit_file", "edit_symbol", "batch_edit", "move_symbol":
		return true
	case "edit", "refactor":
		// Match the facade's normalization/inference/defaults too: an
		// omitted operation with a file/symbol/changes selector is callable.
		operation := resolveFacadeOperationAlias(req.Params.Name, normalizeFacadeOperation(req.GetString("operation", "")))
		if operation == "" {
			operation = inferFacadeOperation(req.Params.Name, req.GetArguments())
		}
		if operation == "" {
			operation = defaultFacadeOperation(req.Params.Name)
		}
		if req.Params.Name == "refactor" {
			return operation == "move"
		}
		switch operation {
		case "write", "file", "symbol", "batch":
			return true
		}
	}
	return false
}

func editOperationFrom(ctx context.Context) *editOperationWitness {
	w, _ := ctx.Value(editOperationContextKey{}).(*editOperationWitness)
	return w
}

// observeEditSourceEffect records a successful forward source write/removal.
// Rollback/journal writes must not call it. Unchanged rewrites are excluded;
// unavailable prior bytes remain unknown instead of being guessed equal/changed.
func observeEditSourceEffect(ctx context.Context, changed, comparisonKnown bool) {
	editOperationFrom(ctx).observeSourceEffect(changed, comparisonKnown)
}

func observeEditSourceReplacement(ctx context.Context, before, after []byte, existed, comparisonKnown bool) {
	w := editOperationFrom(ctx)
	if w == nil {
		return // no extra byte comparison for an unobserved/raw caller
	}
	w.observeSourceEffect(!existed || !bytes.Equal(before, after), comparisonKnown)
}

func (w *editOperationWitness) observeSourceEffect(changed, comparisonKnown bool) {
	if w == nil || (comparisonKnown && !changed) {
		return
	}
	flags := editSourceEffect
	if !comparisonKnown {
		flags |= editSourceUnknownComparison
	}
	w.flags.Or(flags)
}

func observeEditSourceComplete(ctx context.Context) {
	if w := editOperationFrom(ctx); w != nil {
		w.flags.Or(editSourceComplete)
	}
}

// The batch's typed terminal receipt supplies these facts; text formatting and
// err==nil cannot distinguish committed bytes with a journal warning from an
// aborted/rolled-back/conflicted transaction.
func observeEditSourceTerminal(ctx context.Context, committed, warning, rolledBack bool) {
	w := editOperationFrom(ctx)
	if w == nil {
		return
	}
	var flags uint32
	if committed {
		flags |= editSourceComplete
	}
	if warning {
		flags |= editSourceWarning
	}
	if rolledBack {
		flags |= editSourceRolledBack
	}
	w.flags.Or(flags)
}

func (w *editOperationWitness) finish(result *mcp.CallToolResult, err error) {
	flags := w.flags.Load()
	if flags&editSourceEffect == 0 {
		return // replay/status/dry-run/no-op/refusal before any source effect
	}
	cleanResult := err == nil && result != nil && !result.IsError
	if flags&editSourceComplete != 0 {
		switch {
		case !cleanResult || flags&editSourceWarning != 0:
			w.owner.committedWithError.Add(1)
		case flags&editSourceUnknownComparison != 0:
			w.owner.unknownSourceComparison.Add(1)
		default:
			w.owner.completed.Add(1)
		}
		return
	}
	w.owner.failedAfterSourceEffect.Add(1)
	if flags&editSourceRolledBack != 0 {
		w.owner.rolledBack.Add(1)
	}
}

func (s *Server) observeEditOperationHandler(h mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		if s == nil || !measuredSourceEdit(req) {
			return h(ctx, req)
		}
		return s.runMeasuredEditOperation(ctx, req, h)
	}
}

// runMeasuredEditOperation is called only after measuredSourceEdit succeeded.
// The bound-call observer shares that classification; completion still runs
// inside the actual handler, including after an abandoned caller has returned.
func (s *Server) runMeasuredEditOperation(ctx context.Context, req mcp.CallToolRequest, h mcpserver.ToolHandlerFunc) (result *mcp.CallToolResult, err error) {
	if existing := editOperationFrom(ctx); existing != nil && existing.owner == &s.editOperations {
		return h(ctx, req) // a raw/facade nested handler borrows the owner
	}
	w := &editOperationWitness{owner: &s.editOperations}
	ctx = context.WithValue(ctx, editOperationContextKey{}, w)
	defer func() { w.finish(result, err) }()
	return h(ctx, req)
}
