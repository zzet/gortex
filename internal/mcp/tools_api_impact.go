package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// api_impact is a fused, route-anchored pre-change report. Given a route path
// (or handler file) substring, it composes four existing subsystems — the
// contract registry (route + consumers), contracts.Validate (field-level
// response-shape mismatch), analysis.AnalyzeImpact (the real blast radius:
// callers, processes, test files), and best-effort middleware — into one
// answer to "what breaks if I change this endpoint?". It is the single call an
// agent makes BEFORE modifying an API handler.

type apiImpactShape struct {
	Success []string `json:"success"`
	Error   []string `json:"error,omitempty"`
	Note    string   `json:"note,omitempty"`
}

type apiImpactConsumer struct {
	Name            string   `json:"name"`
	File            string   `json:"file,omitempty"`
	Repo            string   `json:"repo,omitempty"`
	Accesses        []string `json:"accesses,omitempty"`
	AttributionNote string   `json:"attribution_note,omitempty"`
}

type apiImpactMismatch struct {
	Consumer   string `json:"consumer"`
	Field      string `json:"field"`
	Reason     string `json:"reason"`
	Confidence string `json:"confidence"` // high | low
}

type apiImpactSummary struct {
	DirectConsumers     int      `json:"direct_consumers"`
	AffectedFlows       int      `json:"affected_flows"`
	AffectedCallers     int      `json:"affected_callers"`
	TestFilesToRun      []string `json:"test_files_to_run,omitempty"`
	RiskLevel           string   `json:"risk_level"`
	Warning             string   `json:"warning,omitempty"`
	ContractRiskUpgrade string   `json:"contract_risk_upgrade,omitempty"`
}

type apiImpactReport struct {
	Route               string              `json:"route"`
	Method              string              `json:"method,omitempty"`
	Path                string              `json:"path,omitempty"`
	Handler             string              `json:"handler,omitempty"`
	Repo                string              `json:"repo,omitempty"`
	ResponseShape       apiImpactShape      `json:"response_shape"`
	Middleware          []string            `json:"middleware,omitempty"`
	MiddlewareDetection string              `json:"middleware_detection,omitempty"`
	Consumers           []apiImpactConsumer `json:"consumers"`
	Mismatches          []apiImpactMismatch `json:"mismatches,omitempty"`
	ExecutionFlows      []string            `json:"execution_flows,omitempty"`
	ImpactSummary       apiImpactSummary    `json:"impact_summary"`
}

func (s *Server) handleAPIImpact(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	route := strings.TrimSpace(req.GetString("route", ""))
	file := strings.TrimSpace(req.GetString("file", ""))
	if route == "" && file == "" {
		return mcp.NewToolResultError("either route or file is required"), nil
	}
	allowed, err := s.resolveRepoFilter(ctx, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reg := s.effectiveContractRegistry()
	if reg == nil {
		return mcp.NewToolResultError("no contract registry available — index a repo with API routes first"), nil
	}

	providers := resolveRouteTargets(reg, route, file, allowed)
	if len(providers) == 0 {
		target := route
		if target == "" {
			target = file
		}
		return mcp.NewToolResultError(fmt.Sprintf("no route matching %q", target)), nil
	}

	lookup := s.contractShapeLookup(ctx)
	fanout := consumerFileFanout(reg)

	reports := make([]apiImpactReport, 0, len(providers))
	for _, p := range providers {
		reports = append(reports, s.buildAPIImpactReport(ctx, p, reg, lookup, fanout))
	}

	var payload any
	if len(reports) == 1 {
		payload = reports[0]
	} else {
		payload = map[string]any{"routes": reports, "total": len(reports)}
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAPIImpact(reports))
	}
	if s.isTOON(ctx, req) {
		return returnTOON(payload)
	}
	return s.respondJSONOrTOON(ctx, req, payload)
}

// resolveRouteTargets returns provider HTTP contracts whose path matches the
// route substring or whose handler file matches the file substring, deduped by
// canonical contract ID (first wins). Registry contracts carry FLAT meta, so
// this avoids the contract_meta nesting gotcha on graph nodes.
func resolveRouteTargets(reg *contracts.Registry, route, file string, allowed map[string]bool) []contracts.Contract {
	seen := map[string]bool{}
	var out []contracts.Contract
	for _, c := range reg.All() {
		if c.Role != contracts.RoleProvider || c.Type != contracts.ContractHTTP {
			continue
		}
		if allowed != nil && c.RepoPrefix != "" && !allowed[c.RepoPrefix] {
			continue
		}
		path := metaStr(c.Meta, "path")
		match := false
		if route != "" && path != "" && strings.Contains(path, route) {
			match = true
		}
		if file != "" && c.FilePath != "" && strings.Contains(c.FilePath, file) {
			match = true
		}
		if !match || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// consumerFileFanout counts, per consumer file, how many distinct route
// contract IDs that file consumes — the multi-fetch heuristic that downgrades
// a mismatch's confidence (one file fetching many routes is a weaker signal
// that a given field belongs to a given route).
func consumerFileFanout(reg *contracts.Registry) map[string]int {
	perFile := map[string]map[string]bool{}
	for _, c := range reg.All() {
		if c.Role != contracts.RoleConsumer || c.FilePath == "" {
			continue
		}
		if perFile[c.FilePath] == nil {
			perFile[c.FilePath] = map[string]bool{}
		}
		perFile[c.FilePath][c.ID] = true
	}
	out := make(map[string]int, len(perFile))
	for f, ids := range perFile {
		out[f] = len(ids)
	}
	return out
}

func (s *Server) buildAPIImpactReport(ctx context.Context, p contracts.Contract, reg *contracts.Registry, lookup contracts.ShapeLookup, fanout map[string]int) apiImpactReport {
	rep := apiImpactReport{
		Route:   p.ID,
		Method:  metaStr(p.Meta, "method"),
		Path:    metaStr(p.Meta, "path"),
		Handler: p.SymbolID,
		Repo:    p.RepoPrefix,
	}
	rep.ResponseShape = apiImpactShape{Success: envelopeNames(p.Meta)}
	if len(rep.ResponseShape.Success) == 0 {
		rep.ResponseShape.Note = "no response envelope extracted for this route"
	}

	// Consumers = consumer-role contracts sharing this canonical ID (same-repo
	// and cross-repo via the contract matcher's pairing).
	repoToConsumer := map[string]string{}
	for _, c := range reg.ByID(p.ID) {
		if c.Role != contracts.RoleConsumer {
			continue
		}
		name := s.nodeName(ctx, c.SymbolID)
		cons := apiImpactConsumer{
			Name:     name,
			File:     c.FilePath,
			Repo:     c.RepoPrefix,
			Accesses: consumerAccesses(c, lookup),
		}
		if fanout[c.FilePath] > 1 {
			cons.AttributionNote = fmt.Sprintf("file consumes %d routes; field attribution is approximate", fanout[c.FilePath])
		}
		rep.Consumers = append(rep.Consumers, cons)
		if c.RepoPrefix != "" {
			repoToConsumer[c.RepoPrefix] = name
		}
	}
	sort.SliceStable(rep.Consumers, func(i, j int) bool { return rep.Consumers[i].Name < rep.Consumers[j].Name })

	// Mismatches via real field-level shape diffing (contracts.Validate over a
	// sub-registry of just this route + its consumers).
	sub := contracts.NewRegistry()
	sub.Add(p)
	for _, c := range reg.ByID(p.ID) {
		if c.Role == contracts.RoleConsumer {
			sub.Add(c)
		}
	}
	breaking := 0
	for _, issue := range contracts.Validate(sub, lookup) {
		if issue.Field == "" {
			continue
		}
		if issue.Severity == contracts.SeverityBreaking {
			breaking++
		}
		confidence := "low"
		if issue.Severity == contracts.SeverityBreaking {
			confidence = "high"
		}
		consumerName := repoToConsumer[issue.Consumer]
		if consumerName == "" {
			consumerName = issue.Consumer
		}
		// A consumer file fetching many routes weakens per-field attribution.
		reason := issue.Details
		if reason == "" {
			reason = issue.Kind
		}
		rep.Mismatches = append(rep.Mismatches, apiImpactMismatch{
			Consumer:   consumerName,
			Field:      issue.Field,
			Reason:     reason,
			Confidence: confidence,
		})
	}
	sort.SliceStable(rep.Mismatches, func(i, j int) bool {
		if rep.Mismatches[i].Consumer != rep.Mismatches[j].Consumer {
			return rep.Mismatches[i].Consumer < rep.Mismatches[j].Consumer
		}
		return rep.Mismatches[i].Field < rep.Mismatches[j].Field
	})

	// Blast radius + execution flows from the real impact analysis.
	var impact *analysis.ImpactResult
	if p.SymbolID != "" {
		impact = analysis.AnalyzeImpact(s.readerFor(ctx), []string{p.SymbolID}, s.getCommunities(), s.getProcesses())
	}
	if impact != nil {
		rep.ExecutionFlows = impact.AffectedProcesses
		rep.ImpactSummary.AffectedFlows = len(impact.AffectedProcesses)
		rep.ImpactSummary.AffectedCallers = impact.TotalAffected
		rep.ImpactSummary.TestFilesToRun = impact.TestFiles
	}

	// Middleware: best-effort from annotation edges on the handler. Never
	// fabricate — mark unavailable when nothing is extracted.
	rep.Middleware = s.handlerMiddleware(ctx, p.SymbolID)
	if len(rep.Middleware) == 0 {
		rep.MiddlewareDetection = "unavailable: gortex does not extract HTTP middleware chains; none inferable from annotations"
	}

	// Risk fusion: consumer-count tier, bumped on any mismatch, max'd with the
	// blast-radius risk, forced HIGH when the route carries breaking drift.
	impactRisk := analysis.RiskLow
	if impact != nil {
		impactRisk = impact.Risk
	}
	risk, upgrade := fuseRouteRisk(len(rep.Consumers), len(rep.Mismatches), impactRisk, breaking)
	rep.ImpactSummary.DirectConsumers = len(rep.Consumers)
	rep.ImpactSummary.RiskLevel = string(risk)
	rep.ImpactSummary.ContractRiskUpgrade = upgrade
	if len(rep.Consumers) > 0 {
		rep.ImpactSummary.Warning = fmt.Sprintf("changing the response shape of %s will affect %d consumer(s)", p.ID, len(rep.Consumers))
	}
	return rep
}

// fuseRouteRisk combines GitNexus's consumer-count heuristic (>=10 HIGH, >=4
// MEDIUM, else LOW; +1 level on any mismatch) with gortex's real blast-radius
// risk tier, and forces HIGH when the route is a contract boundary with
// existing breaking drift.
func fuseRouteRisk(consumers, mismatches int, impactRisk analysis.RiskLevel, breaking int) (analysis.RiskLevel, string) {
	base := 0 // 0=LOW 1=MEDIUM 2=HIGH
	switch {
	case consumers >= 10:
		base = 2
	case consumers >= 4:
		base = 1
	}
	if mismatches > 0 && base < 2 {
		base++
	}
	if r := riskRank(impactRisk); r > base {
		base = r
	}
	if breaking > 0 {
		return analysis.RiskHigh, "risk raised to HIGH — route is a contract boundary with breaking response-shape drift"
	}
	return riskFromRank(base), ""
}

func riskRank(r analysis.RiskLevel) int {
	switch r {
	case analysis.RiskCritical, analysis.RiskHigh:
		return 2
	case analysis.RiskMedium:
		return 1
	default:
		return 0
	}
}

func riskFromRank(n int) analysis.RiskLevel {
	switch {
	case n >= 2:
		return analysis.RiskHigh
	case n == 1:
		return analysis.RiskMedium
	default:
		return analysis.RiskLow
	}
}

// contractShapeLookup resolves a type symbol ID to its field-level Shape from
// the graph (the same closure handleValidateContracts / computeContractImpact
// use).
func (s *Server) contractShapeLookup(ctx context.Context) contracts.ShapeLookup {
	g := s.readerFor(ctx)
	return contracts.ShapeLookup(func(id string) *contracts.Shape {
		n := g.GetNode(id)
		if n == nil || n.Meta == nil {
			return nil
		}
		switch v := n.Meta["shape"].(type) {
		case *contracts.Shape:
			return v
		case contracts.Shape:
			return &v
		}
		return nil
	})
}

// consumerAccesses returns the field names a consumer expects from the route's
// response — the fields of the consumer contract's response_type shape.
func consumerAccesses(c contracts.Contract, lookup contracts.ShapeLookup) []string {
	respType := metaStr(c.Meta, "response_type")
	if respType == "" || lookup == nil {
		return nil
	}
	sh := lookup(respType)
	if sh == nil {
		return nil
	}
	names := make([]string, 0, len(sh.Fields))
	for _, f := range sh.Fields {
		if f.Name != "" {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names
}

// handlerMiddleware collects annotation targets on a handler symbol as a
// best-effort middleware list (decorators like @UseGuards / withAuth).
func (s *Server) handlerMiddleware(ctx context.Context, symbolID string) []string {
	if symbolID == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range s.readerFor(ctx).GetOutEdges(symbolID) {
		if e.Kind != graph.EdgeAnnotated {
			continue
		}
		name := s.nodeName(ctx, e.To)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// envelopeNames extracts the success response key names from a route's flat
// response_envelope meta ([]map with a "name" key).
func envelopeNames(meta map[string]any) []string {
	raw, ok := meta["response_envelope"]
	if !ok {
		return nil
	}
	var names []string
	switch env := raw.(type) {
	case []map[string]any:
		for _, row := range env {
			if n, _ := row["name"].(string); n != "" {
				names = append(names, n)
			}
		}
	case []any:
		for _, r := range env {
			if row, ok := r.(map[string]any); ok {
				if n, _ := row["name"].(string); n != "" {
					names = append(names, n)
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

func metaStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func (s *Server) nodeName(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if n := s.readerFor(ctx).GetNode(id); n != nil && n.Name != "" {
		return n.Name
	}
	if i := strings.LastIndex(id, "::"); i >= 0 {
		return id[i+2:]
	}
	return id
}
