package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

// QRT-6: a risk-gated modification guard. Load-bearing symbols (high fan-in /
// high centrality) require a prior, acknowledged impact review with a TTL.
// The ack is not a side file — it is a development memory keyed on the symbol,
// so it is graph state that survives daemon restarts and surfaces for the next
// agent. The gate emits a refuse verdict; a pretooluse hook enforces it.

const (
	riskAckTag            = "risk-ack"
	riskGateCallerDefault = 8
	riskGatePRThreshold   = 0.66
	defaultRiskAckTTL     = 4 * time.Hour
	riskGateMemImportance = 4
)

// riskGateEnabled reports whether the per-symbol risk gate is active for this
// call — either the request opted in or GORTEX_RISK_GATE is set.
func riskGateEnabled(req mcp.CallToolRequest) bool {
	if req.GetBool("risk_gate", false) {
		return true
	}
	switch os.Getenv("GORTEX_RISK_GATE") {
	case "", "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// riskAckTTL is how long a recorded ack stays fresh. Overridable via
// GORTEX_RISK_ACK_TTL (a Go duration like "2h").
func riskAckTTL() time.Duration {
	if v := os.Getenv("GORTEX_RISK_ACK_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultRiskAckTTL
}

func riskGateCallerThreshold() int {
	if v := os.Getenv("GORTEX_RISK_GATE_MIN_CALLERS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return riskGateCallerDefault
}

// riskGatedSymbols returns the changed symbols that require an ack.
func (s *Server) riskGatedSymbols(ctx context.Context, p *prediction) []*graph.Node {
	if p == nil || len(p.nodes) == 0 {
		return nil
	}
	fanIn, _ := computeFanInOut(s.readerFor(ctx), p.nodes)
	ids := make([]string, 0, len(p.nodes))
	for _, node := range p.nodes {
		if node != nil {
			ids = append(ids, node.ID)
		}
	}
	pageRank := make(map[string]float64, len(ids))
	if metrics, err := s.analysisNodeMetricsBatched(ids); err == nil {
		for _, metric := range metrics {
			pageRank[metric.NodeID] = metric.PageRank
		}
	}
	maxPR := s.topAnalysisMetricValue(graph.AnalysisMetricPageRank)
	out := make([]*graph.Node, 0, len(p.nodes))
	for _, node := range p.nodes {
		if node == nil {
			continue
		}
		gated := fanIn[node.ID] >= riskGateCallerThreshold()
		if !gated && maxPR > 0 {
			gated = pageRank[node.ID]/maxPR >= riskGatePRThreshold
		}
		if gated {
			out = append(out, node)
		}
	}
	return out
}

// hasFreshAck reports whether a non-expired risk-ack memory exists for a symbol.
func (s *Server) hasFreshAck(id string) bool {
	store := s.resolveMemoryStore("workspace")
	if store == nil {
		return false
	}
	ttl := riskAckTTL()
	for _, m := range store.Query(MemoryQueryFilter{Tag: riskAckTag, SymbolID: id}) {
		if time.Since(m.UpdatedAt) < ttl {
			return true
		}
	}
	return false
}

// recordRiskAck stores (or refreshes) the ack memory for a symbol.
func (s *Server) recordRiskAck(id, author string, risk changeRisk) (string, error) {
	store := s.resolveMemoryStore("workspace")
	if store == nil {
		return "", fmt.Errorf("no memory store available to record the ack")
	}
	body := fmt.Sprintf("Risk-gate ack for %s — change impact reviewed (risk score %d, blast %d). Valid for %s.",
		id, risk.Score, risk.BlastSize, riskAckTTL())
	return store.Save(persistence.MemoryEntry{
		Kind:        "constraint",
		Source:      "change_contract",
		Body:        body,
		Tags:        []string{riskAckTag},
		SymbolIDs:   []string{id},
		Importance:  riskGateMemImportance,
		AuthorAgent: author,
	})
}

// riskGateReasons checks each gated symbol for a fresh ack and returns refuse
// reasons for those lacking one.
func (s *Server) riskGateReasons(ctx context.Context, p *prediction, risk changeRisk) []changeReason {
	var reasons []changeReason
	for _, n := range s.riskGatedSymbols(ctx, p) {
		if s.hasFreshAck(n.ID) {
			reasons = append(reasons, changeReason{
				Family:     "risk_gate",
				Severity:   "info",
				Message:    fmt.Sprintf("%s is risk-gated and has a fresh ack — cleared", n.Name),
				Confidence: 1,
				Symbol:     n.ID,
			})
			continue
		}
		reasons = append(reasons, changeReason{
			Family:     "risk_gate",
			Severity:   "error",
			Message:    fmt.Sprintf("%s is risk-gated (load-bearing) and has no fresh impact-review ack — acknowledge with `change_contract ack:true symbols:%s` (ack valid %s)", n.Name, n.ID, riskAckTTL()),
			Confidence: 0.9,
			Symbol:     n.ID,
		})
	}
	return reasons
}

// handleRiskAck records acks for the changed set and returns a confirmation.
func (s *Server) handleRiskAck(ctx context.Context, req mcp.CallToolRequest, p *prediction) (*mcp.CallToolResult, error) {
	author := s.ackAuthor(ctx)
	risk := s.scoreChangeRisk(p)

	// Ack every changed symbol the caller named (an explicit acknowledgement
	// of this change set), so a subsequent gated run on any of them clears.
	ids := p.changedIDs
	if len(ids) == 0 {
		return mcp.NewToolResultError("change_contract ack: no changed symbols to acknowledge"), nil
	}
	acked := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := s.recordRiskAck(id, author, risk); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		acked = append(acked, id)
	}
	sort.Strings(acked)
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"acked":      acked,
		"ttl":        riskAckTTL().String(),
		"expires_at": time.Now().Add(riskAckTTL()).UTC().Format(time.RFC3339),
		"note":       "Risk-gate acks recorded as development memories. A change_contract run with risk_gate:true now clears these symbols until the TTL elapses.",
	})
}

// ackAuthor returns the MCP client name for ack provenance, or a default.
func (s *Server) ackAuthor(ctx context.Context) string {
	if sess := s.sessionFor(ctx); sess != nil {
		if n := sess.snapshotClientName(); n != "" {
			return n
		}
	}
	return "change_contract"
}
