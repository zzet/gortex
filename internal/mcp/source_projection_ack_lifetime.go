package mcp

import (
	"context"
	"sync"
	"weak"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Projection lifetime is independent of the five measured edit surfaces. The
// existing source-effect registry includes raw/facade rename and cohort writers
// without broadening acknowledgement clocks or logical edit counters.
type projectionAcknowledgement struct {
	owner  *Server
	mu     sync.Mutex
	closed bool
	holds  []projectionAcknowledgementHold
}

type projectionAcknowledgementKey struct{}

type projectionAcknowledgementHold struct {
	registry *pendingProjections
	entry    weak.Pointer[projectionEntry]
}

func projectionAcknowledgementFrom(ctx context.Context, owner *Server) *projectionAcknowledgement {
	pending, _ := ctx.Value(projectionAcknowledgementKey{}).(*projectionAcknowledgement)
	if pending != nil && pending.owner == owner {
		return pending
	}
	return nil
}

// The wrapper runs inside the existing hang firewall. Caller abandonment does
// not release the actual handler's hold; error/panic/actual completion do. A
// same-Server nested facade/legacy invocation borrows the original owner.
func (s *Server) withProjectionAcknowledgement(ctx context.Context, req mcplib.CallToolRequest, h mcpserver.ToolHandlerFunc) (context.Context, mcpserver.ToolHandlerFunc) {
	if s == nil || !s.facades.mutatesSource(req.Params.Name) || projectionAcknowledgementFrom(ctx, s) != nil {
		return ctx, h
	}
	pending := &projectionAcknowledgement{owner: s}
	ctx = context.WithValue(ctx, projectionAcknowledgementKey{}, pending)
	return ctx, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		defer pending.finish()
		return h(ctx, req)
	}
}

// entry is still private. Establish the weak hold before single/cohort
// publication makes it visible to concurrent retirement checks. The bounded
// registry alone owns file graphs; no receipt or TTL ledger retains this scope.
func holdProjectionForAcknowledgement(pending *projectionAcknowledgement, registry *pendingProjections, entry *projectionEntry) {
	if pending == nil {
		return
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if pending.closed {
		return
	}
	entry.acknowledgementPending = true
	pending.holds = append(pending.holds, projectionAcknowledgementHold{registry: registry, entry: weak.Make(entry)})
}

func (a *projectionAcknowledgement) finish() {
	a.mu.Lock()
	a.closed = true
	holds := a.holds
	a.holds = nil
	a.mu.Unlock()
	for _, hold := range holds {
		if entry := hold.entry.Value(); entry != nil {
			hold.registry.releaseProjectionAcknowledgement(entry)
		}
	}
}

func (p *pendingProjections) releaseProjectionAcknowledgement(entry *projectionEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry.acknowledgementPending = false
	if !entry.acknowledgementRetirement {
		return
	}
	entry.acknowledgementRetirement = false
	// The original verification is permission to drop only the same unchanged
	// owner. It is not live ACK evidence and cannot follow a newer publication.
	if p.snap.Load().entries[entry.key] != entry || entry.sourceEvents != entry.acknowledgementRevision || entry.suspect.Load() {
		return
	}
	entry.checkedRetired.Store(true)
	p.retired.Add(int64(p.dropAllLocked([]*projectionEntry{entry})))
}
