package daemon

import "os"

const mcpDispatchOccupancyEnv = "GORTEX_MCP_DISPATCH_OCCUPANCY"

type mcpDispatchOccupant uint8

const (
	mcpOccupantOther mcpDispatchOccupant = iota
	mcpOccupantProtocol
	mcpOccupantEdit
	mcpOccupantReceipt
	mcpOccupantSource
	mcpOccupantSummary
	mcpOccupantEditingContext
	mcpOccupantSymbols
	mcpOccupantText
	mcpOccupantCallers
	mcpOccupantCallChain
	mcpOccupantCount
)

var mcpOccupantNames = [mcpOccupantCount]string{
	"other", "protocol", "edit", "change.receipt", "read.source", "read.summary",
	"read.editing_context", "search.symbols", "search.text", "relations.callers", "trace.call_chain",
}

func mcpDispatchOccupancyEnabled() bool {
	return os.Getenv(mcpDispatchOccupancyEnv) == "1"
}

// The diagnostic recognizes only the bounded stock edit and probe vocabulary.
// Unknown tools and aliases share one bucket. No user argument is retained.
func mcpDispatchOccupantFor(name string, args map[string]any) mcpDispatchOccupant {
	operation, _ := args["operation"].(string)
	switch name {
	case "edit", "edit_file", "batch_edit":
		return mcpOccupantEdit
	case "mutation_status":
		return mcpOccupantReceipt
	case "get_symbol_source":
		return mcpOccupantSource
	case "get_file_summary":
		return mcpOccupantSummary
	case "get_editing_context":
		return mcpOccupantEditingContext
	case "search_symbols":
		return mcpOccupantSymbols
	case "search_text":
		return mcpOccupantText
	case "get_callers":
		return mcpOccupantCallers
	case "get_call_chain":
		return mcpOccupantCallChain
	case "change":
		if operation == "receipt" {
			return mcpOccupantReceipt
		}
	case "read":
		switch operation {
		case "source":
			return mcpOccupantSource
		case "summary":
			return mcpOccupantSummary
		case "editing_context":
			return mcpOccupantEditingContext
		}
	case "search":
		switch operation {
		case "symbols":
			return mcpOccupantSymbols
		case "text":
			return mcpOccupantText
		}
	case "relations":
		if operation == "callers" {
			return mcpOccupantCallers
		}
	case "trace":
		if operation == "call_chain" {
			return mcpOccupantCallChain
		}
	}
	return mcpOccupantOther
}

type mcpDispatchOccupancy struct {
	reason          string
	usedSlots       int
	totalSlots      int
	queuedInLane    int
	requestEligible bool
	counts          [mcpOccupantCount]int
}

func (s *mcpDispatchScheduler) refusalOccupancyLocked(class MCPDispatchClass, session, reason string) *mcpDispatchOccupancy {
	if !s.owner.observeOccupancy {
		return nil
	}
	// The protocol path can add a token while the owner lock is held. Use
	// one channel sample for both occupancy and the eligibility decision.
	usedSlots := len(s.slots)
	return &mcpDispatchOccupancy{
		reason: reason, usedSlots: usedSlots, totalSlots: cap(s.slots),
		queuedInLane: s.lanes[class].queued, requestEligible: s.eligibleAtOccupancyLocked(class, session, usedSlots),
		counts: s.occupants,
	}
}

func (o *mcpDispatchOccupancy) wire(class MCPDispatchClass) map[string]any {
	counts := make(map[string]int)
	accounted := 0
	for index, count := range o.counts {
		if count > 0 {
			counts[mcpOccupantNames[index]] = count
			accounted += count
		}
	}
	return map[string]any{
		"reason": o.reason, "lane": class.String(), "used_slots": o.usedSlots,
		"total_slots": o.totalSlots, "queued_in_lane": o.queuedInLane,
		"request_eligible": o.requestEligible, "in_flight": counts,
		// Protocol admission can acquire a channel token just before it
		// takes the owner mutex to record that grant.
		"unattributed_slots": max(0, o.usedSlots-accounted),
	}
}
