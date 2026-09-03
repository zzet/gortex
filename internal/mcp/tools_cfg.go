package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	wire "github.com/gortexhq/gcx-go"
	"github.com/zzet/gortex/internal/cfg"
	"github.com/zzet/gortex/internal/graph"
)

// registerCFGTools wires the control-flow surface: get_cfg returns a
// function's basic blocks, labeled edges, per-statement def/use sets
// and reaching-definition chains — built on demand from the symbol's
// source, never at index time.
func (s *Server) registerCFGTools() {
	s.addTool(
		mcp.NewTool("get_cfg",
			mcp.WithDescription("Builds the intra-procedural control-flow graph for one function/method: basic blocks (ordered statements with line spans and per-statement def/use variable sets), labeled edges (seq / true / false / loop_back / break / continue / return / case / exception / finally), and statement-granular def→use chains from a GEN/KILL reaching-definitions fixpoint. Supports Go, Python, JavaScript, TypeScript, Java, Rust, Ruby. Pass mermaid:true for a Mermaid flowchart rendering. Built on demand from the symbol's source — pairs with flow_between (where a value flows across symbols) by answering how values move inside one function."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Function or method symbol node ID")),
			mcp.WithBoolean("mermaid", mcp.Description("Include a Mermaid flowchart rendering of the block graph (default: false)")),
			mcp.WithString("format", mcp.Description("Output format: json (default), gcx (GCX1 compact wire format), or toon")),
			mcp.WithNumber("max_bytes", mcp.Description("Cap the marshaled response at this many bytes. The longest list is trimmed; truncation metadata rides on the response. Omit for no cap.")),
		),
		s.handleGetCFG,
	)
}

// symbolCFG is the resolved bundle both get_cfg and analyze def_use
// consume: the graph node plus its freshly built CFG and reaching-
// definitions result.
type symbolCFG struct {
	node  *graph.Node
	graph *cfg.CFG
	reach *cfg.ReachingResult
}

// buildSymbolCFG fetches a function/method node, slices its source
// out of the owning repo's file (overlay-aware), and builds the CFG.
// Errors are caller-facing strings, suitable for tool results.
func (s *Server) buildSymbolCFG(ctx context.Context, id string) (*symbolCFG, error) {
	node := s.engineFor(ctx).GetSymbol(id)
	if node == nil {
		return nil, fmt.Errorf("symbol not found: %s", id)
	}
	if !s.nodeInSessionScope(ctx, node) {
		return nil, fmt.Errorf("symbol not found: %s", id)
	}
	if node.Kind != graph.KindFunction && node.Kind != graph.KindMethod {
		return nil, fmt.Errorf("symbol %s is a %s — get_cfg needs a function or method", id, node.Kind)
	}
	if !cfg.SupportedLanguage(node.Language) {
		return nil, fmt.Errorf("control-flow graphs are not supported for language %q (supported: go, python, javascript, typescript, java, rust, ruby)", node.Language)
	}
	if node.StartLine == 0 || node.EndLine == 0 {
		return nil, fmt.Errorf("symbol has no line range: %s", id)
	}
	absPath, err := s.resolveNodePath(ctx, node)
	if err != nil {
		return nil, err
	}
	source, fromLine, _, err := s.readLinesForCtx(ctx, absPath, node.StartLine, node.EndLine, 0)
	if err != nil {
		return nil, fmt.Errorf("could not read source: %v", err)
	}
	c, err := cfg.Build([]byte(source), node.Language, cfg.Options{
		LineOffset: fromLine - 1,
		FuncName:   node.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("cfg build failed for %s: %v", id, err)
	}
	return &symbolCFG{node: node, graph: c, reach: c.ReachingDefinitions()}, nil
}

func (s *Server) handleGetCFG(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := s.symbolIDArg(ctx, req)
	if err != nil {
		return mcp.NewToolResultError("id is required"), nil
	}

	// Auto re-index stale file before querying.
	if parts := strings.SplitN(id, "::", 2); len(parts) == 2 {
		s.ensureFresh([]string{parts[0]})
	}

	sc, buildErr := s.buildSymbolCFG(ctx, id)
	if buildErr != nil {
		return mcp.NewToolResultError(buildErr.Error()), nil
	}
	sess := s.sessionFor(ctx)
	sess.recordSymbol(id)
	sess.recordFile(sc.node.FilePath)

	wantMermaid := req.GetBool("mermaid", false)

	if s.isGCX(ctx, req) {
		payload, encErr := encodeGetCFG(sc, wantMermaid)
		return s.gcxResponseWithBudget(req)(payload, encErr)
	}

	blocks := make([]map[string]any, 0, len(sc.graph.Blocks))
	for _, bl := range sc.graph.Blocks {
		stmts := bl.Stmts
		if stmts == nil {
			stmts = []*cfg.Statement{}
		}
		blocks = append(blocks, map[string]any{
			"id":         bl.ID,
			"label":      bl.Label,
			"statements": stmts,
		})
	}
	result := map[string]any{
		"id":           sc.node.ID,
		"name":         sc.graph.FuncName,
		"kind":         string(sc.node.Kind),
		"language":     sc.graph.Language,
		"file_path":    sc.node.FilePath,
		"start_line":   sc.node.StartLine,
		"end_line":     sc.node.EndLine,
		"entry":        sc.graph.Entry,
		"exit":         sc.graph.Exit,
		"blocks":       blocks,
		"edges":        sc.graph.Edges,
		"def_use":      sc.reach.Chains,
		"total_blocks": len(sc.graph.Blocks),
		"total_edges":  len(sc.graph.Edges),
	}
	if wantMermaid {
		result["mermaid"] = sc.graph.Mermaid()
	}
	if s.isTOON(ctx, req) {
		return returnTOON(result)
	}
	return s.respondJSONOrTOON(ctx, req, result)
}

// encodeGetCFG emits a GCX1 envelope with four sections —
// get_cfg.summary (one row), get_cfg.stmts (one row per statement
// with its block, span, kind and def/use sets), get_cfg.edges and
// get_cfg.chains — plus an optional get_cfg.mermaid section.
func encodeGetCFG(sc *symbolCFG, wantMermaid bool) ([]byte, error) {
	var buf bytes.Buffer
	sumEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "get_cfg.summary",
		Fields: []string{"id", "name", "language", "file", "entry", "exit", "blocks", "edges", "stmts", "chains"},
	})
	if err := sumEnc.WriteRow(sc.node.ID, sc.graph.FuncName, sc.graph.Language, sc.node.FilePath,
		sc.graph.Entry, sc.graph.Exit, len(sc.graph.Blocks), len(sc.graph.Edges),
		len(sc.graph.Stmts), len(sc.reach.Chains)); err != nil {
		return nil, err
	}
	if err := sumEnc.Close(); err != nil {
		return nil, err
	}

	stmtEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "get_cfg.stmts",
		Fields: []string{"index", "block", "start_line", "end_line", "kind", "defs", "uses", "text"},
		Meta:   map[string]string{"count": fmt.Sprintf("%d", len(sc.graph.Stmts))},
	})
	for _, st := range sc.graph.Stmts {
		if err := stmtEnc.WriteRow(st.Index, st.Block, st.StartLine, st.EndLine, st.Kind,
			strings.Join(st.Defs, ","), strings.Join(st.Uses, ","), st.Text); err != nil {
			return nil, err
		}
	}
	if err := stmtEnc.Close(); err != nil {
		return nil, err
	}

	edgeEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "get_cfg.edges",
		Fields: []string{"from", "to", "label"},
		Meta:   map[string]string{"count": fmt.Sprintf("%d", len(sc.graph.Edges))},
	})
	for _, e := range sc.graph.Edges {
		if err := edgeEnc.WriteRow(e.From, e.To, string(e.Label)); err != nil {
			return nil, err
		}
	}
	if err := edgeEnc.Close(); err != nil {
		return nil, err
	}

	chainEnc := wire.NewEncoder(&buf, wire.Header{
		Tool:   "get_cfg.chains",
		Fields: []string{"stmt", "var", "defs"},
		Meta:   map[string]string{"count": fmt.Sprintf("%d", len(sc.reach.Chains))},
	})
	for _, ch := range sc.reach.Chains {
		if err := chainEnc.WriteRow(ch.Stmt, ch.Var, joinInts(ch.Defs)); err != nil {
			return nil, err
		}
	}
	if err := chainEnc.Close(); err != nil {
		return nil, err
	}

	if wantMermaid {
		mEnc := wire.NewEncoder(&buf, wire.Header{
			Tool:   "get_cfg.mermaid",
			Fields: []string{"diagram"},
		})
		if err := mEnc.WriteRow(sc.graph.Mermaid()); err != nil {
			return nil, err
		}
		if err := mEnc.Close(); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// analyze kind=def_use
// ---------------------------------------------------------------------------

// defUseItem is the GCX row shape for analyze def_use: one row per
// def→use chain, flattened to lines so consumers don't need the
// block table.
type defUseItem struct {
	Symbol   string
	Var      string
	UseLine  int
	UseText  string
	DefLines string
}

// handleAnalyzeDefUse computes statement-granular def→use chains and
// a per-variable reaching-definition summary for the requested
// function/method symbols. `ids` is a comma-separated ID list (or a
// JSON array); `id` works for a single symbol. Symbols that can't be
// analyzed (wrong kind, unsupported language, unreadable source)
// degrade to a per-symbol error instead of failing the whole call.
func (s *Server) handleAnalyzeDefUse(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ids := symbolIDList(req.GetArguments())
	if len(ids) == 0 {
		// Name the public selector first: the compact surface is what
		// capabilities advertises, so an error that only speaks legacy
		// vocabulary sends the caller looking for a field their schema
		// does not have.
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   "def_use requires a target symbol — pass target:{symbol:\"<symbol id>\"} (legacy: `id`, or `ids` for a comma-separated list)",
			Data:      map[string]any{"field": "target.symbol", "kind": "def_use"},
		}), nil
	}

	type chainRow struct {
		Stmt     int    `json:"stmt"`
		StmtLine int    `json:"stmt_line"`
		StmtText string `json:"stmt_text"`
		Var      string `json:"var"`
		Defs     []int  `json:"defs"`
		DefLines []int  `json:"def_lines"`
	}
	type varRow struct {
		Var      string `json:"var"`
		Defs     int    `json:"defs"`
		Uses     int    `json:"uses"`
		DefLines []int  `json:"def_lines"`
	}
	type symbolRow struct {
		ID        string     `json:"id"`
		Name      string     `json:"name,omitempty"`
		File      string     `json:"file,omitempty"`
		Language  string     `json:"language,omitempty"`
		Chains    []chainRow `json:"chains,omitempty"`
		Variables []varRow   `json:"variables,omitempty"`
		Error     string     `json:"error,omitempty"`
	}

	rows := make([]symbolRow, 0, len(ids))
	var gcxItems []defUseItem
	for _, id := range ids {
		sc, err := s.buildSymbolCFG(ctx, id)
		if err != nil {
			rows = append(rows, symbolRow{ID: id, Error: err.Error()})
			continue
		}
		row := symbolRow{
			ID:       sc.node.ID,
			Name:     sc.graph.FuncName,
			File:     sc.node.FilePath,
			Language: sc.graph.Language,
		}
		lineOf := func(stmt int) int { return sc.graph.Stmts[stmt].StartLine }
		for _, ch := range sc.reach.Chains {
			defLines := make([]int, len(ch.Defs))
			for i, d := range ch.Defs {
				defLines[i] = lineOf(d)
			}
			row.Chains = append(row.Chains, chainRow{
				Stmt:     ch.Stmt,
				StmtLine: lineOf(ch.Stmt),
				StmtText: sc.graph.Stmts[ch.Stmt].Text,
				Var:      ch.Var,
				Defs:     ch.Defs,
				DefLines: defLines,
			})
			gcxItems = append(gcxItems, defUseItem{
				Symbol:   sc.node.ID,
				Var:      ch.Var,
				UseLine:  lineOf(ch.Stmt),
				UseText:  sc.graph.Stmts[ch.Stmt].Text,
				DefLines: joinInts(defLines),
			})
		}
		// Per-variable rollup: definition sites and read counts.
		defLines := map[string][]int{}
		useCount := map[string]int{}
		for _, d := range sc.reach.Defs {
			defLines[d.Var] = append(defLines[d.Var], lineOf(d.Stmt))
		}
		for _, st := range sc.graph.Stmts {
			for _, u := range st.Uses {
				useCount[u]++
			}
		}
		vars := make([]string, 0, len(defLines))
		for v := range defLines {
			vars = append(vars, v)
		}
		sort.Strings(vars)
		for _, v := range vars {
			row.Variables = append(row.Variables, varRow{
				Var:      v,
				Defs:     len(defLines[v]),
				Uses:     useCount[v],
				DefLines: defLines[v],
			})
		}
		rows = append(rows, row)
	}

	if s.isGCX(ctx, req) {
		return s.gcxResponseWithBudget(req)(encodeAnalyze("def_use", gcxItems))
	}
	if isCompact(req) {
		var b strings.Builder
		for _, r := range rows {
			if r.Error != "" {
				fmt.Fprintf(&b, "%s  ERROR %s\n", r.ID, r.Error)
				continue
			}
			for _, ch := range r.Chains {
				fmt.Fprintf(&b, "%s:%d  %s <- defs at %s\n", r.File, ch.StmtLine, ch.Var, joinInts(ch.DefLines))
			}
		}
		if b.Len() == 0 {
			b.WriteString("no def->use chains\n")
		}
		return mcp.NewToolResultText(b.String()), nil
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"symbols": rows,
		"total":   len(rows),
	})
}

// symbolIDList parses the def_use id arguments: `ids` as a comma-
// separated string or JSON array, falling back to a single `id`.
// splitSymbolIDField decodes every encoding a symbol-id list arrives in: a
// real array, the legacy comma-separated string, or the JSON array the facade
// emits when it lowers target:{symbols}. Comma-splitting a JSON array yields
// bracket- and quote-fringed ids that match nothing, which reads as an empty
// result rather than an error — so the reader, not the writer, is where the
// encodings have to converge.
func splitSymbolIDField(raw any) []string {
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	switch value := raw.(type) {
	case string:
		if trimmed := strings.TrimSpace(value); strings.HasPrefix(trimmed, "[") {
			var decoded []string
			if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
				for _, item := range decoded {
					add(item)
				}
				return out
			}
		}
		for _, part := range strings.Split(value, ",") {
			add(part)
		}
	case []string:
		for _, item := range value {
			add(item)
		}
	case []any:
		for _, item := range value {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	}
	return out
}

func symbolIDList(args map[string]any) []string {
	out := splitSymbolIDField(args["ids"])
	if len(out) == 0 {
		if id, ok := args["id"].(string); ok {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}
