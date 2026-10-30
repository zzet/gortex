package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const editProbeScenario = "edit-and-probe"
const editProbeMean = 15 * time.Second
const editProbePeriod = 25 * time.Millisecond

type editProbeStep struct {
	Ordinal      int     `json:"ordinal"`
	ActivateNS   int64   `json:"activate_ns"`
	DueNS        int64   `json:"due_ns"`
	Marker       string  `json:"marker"`
	MutationID   string  `json:"mutation_id"`
	Request      request `json:"request"`
	RequestHash  string  `json:"request_sha256"`
	BeforeSymbol string  `json:"before_symbol,omitempty"`
}

type editProbePlan struct {
	Version       int             `json:"version"`
	Arrival       string          `json:"arrival"`
	MeanNS        int64           `json:"mean_interarrival_ns"`
	ProbePeriodNS int64           `json:"probe_period_ns"`
	Count         int             `json:"count"`
	Seed          int64           `json:"seed"`
	Kind          string          `json:"kind"`
	Tool          string          `json:"tool"`
	Language      string          `json:"language"`
	Mode          string          `json:"mode"`
	Repo          string          `json:"repo"`
	Commit        string          `json:"commit"`
	File          string          `json:"file"`
	Callee        string          `json:"callee"`
	SessionModel  string          `json:"session_model"`
	NotSelected   []string        `json:"not_selected_not_tested"`
	Steps         []editProbeStep `json:"steps"`
}

func makeEditProbePlan(c Corpus, seed int64, count int) (editProbePlan, error) {
	if err := validateCorpus(c); err != nil {
		return editProbePlan{}, err
	}
	if count != 1 && count != 100 {
		return editProbePlan{}, errors.New("edit-and-probe requires 100 edits, or explicit 1-edit diagnostic")
	}
	tool, err := editProbeTool(c)
	if err != nil {
		return editProbePlan{}, err
	}
	kind := c.EditKind
	if kind == "" {
		kind = "add_intrafile_call"
	}
	if kind != "add_intrafile_call" && kind != "rename" {
		return editProbePlan{}, errors.New("unsupported edit kind")
	}
	if kind != "add_intrafile_call" && tool != "edit.symbol" {
		return editProbePlan{}, errors.New("rename fixtures currently require explicit edit.symbol")
	}
	language := "go"
	if filepath.Ext(c.File) == ".ts" {
		language = "typescript"
	}
	if filepath.Ext(c.File) == ".py" {
		language = "python"
	}
	if (filepath.Ext(c.File) != ".go" && filepath.Ext(c.File) != ".ts" && filepath.Ext(c.File) != ".py") || c.EditAnchor == "" || strings.Count(c.EditTemplate, "{{marker}}") != 1 {
		return editProbePlan{}, errors.New("edit-and-probe cell requires Go or explicit TypeScript/Python fixture/stable anchor/one marker declaration")
	}
	separator := strings.LastIndex(c.Symbol, "::")
	if separator < 0 {
		return editProbePlan{}, errors.New("initial intrafile cell requires one plain callee symbol")
	}
	callee := c.Symbol[separator+2:]
	if !token.IsIdentifier(callee) {
		return editProbePlan{}, errors.New("initial intrafile cell requires one plain callee symbol")
	}
	probeMarker := "GortexEditProbeDeclaration"
	switch language {
	case "typescript":
		valid, correct := editProbeTypeScriptFixture(strings.ReplaceAll(c.EditTemplate, "{{marker}}", probeMarker), probeMarker, callee)
		if !valid || !correct {
			return editProbePlan{}, errors.New("TypeScript template must be a numeric zero-argument marker function returning the intrafile call")
		}
	case "python":
		valid, correct := editProbePythonFixture(strings.ReplaceAll(c.EditTemplate, "{{marker}}", probeMarker), probeMarker, callee)
		if !valid || !correct {
			return editProbePlan{}, errors.New("python template must be a top-level marker with one indented return of the intrafile call")
		}
	default:
		parsed, err := parser.ParseFile(token.NewFileSet(), "template.go", "package probe\n"+strings.ReplaceAll(c.EditTemplate, "{{marker}}", probeMarker), 0)
		if err != nil {
			return editProbePlan{}, fmt.Errorf("edit template is not standalone Go declarations: %w", err)
		}
		declaration, call := false, false
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == probeMarker && fn.Body != nil {
				declaration = true
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if expr, ok := n.(*ast.CallExpr); ok {
						if name, ok := expr.Fun.(*ast.Ident); ok && name.Name == callee {
							call = true
						}
					}
					return true
				})
			}
		}
		if !declaration || !call {
			return editProbePlan{}, errors.New("template must declare the unique marker function and actually call the selected intrafile callee")
		}
	}
	p := editProbePlan{Version: 1, Arrival: "precomputed_open_loop_poisson", MeanNS: int64(editProbeMean), ProbePeriodNS: int64(editProbePeriod), Count: count, Seed: seed, Kind: kind, Tool: tool, Language: language, Mode: "json", Repo: c.Label, Commit: c.Commit, File: c.File, Callee: callee, SessionModel: "one_editor_plus_one_separate_prober_per_edit", NotSelected: []string{"other edit kinds/tools/languages/output modes", "native save/create/delete/hotfile/restart", "largest-file/reference configurations require their own reviewed manifest"}}
	rng := rand.New(rand.NewPCG(uint64(seed)^0xa0761d6478bd642f, uint64(seed)^0xe7037ed1a0b428db))
	due := int64(editProbePeriod)
	writeContent := c.EditInitialContent
	for i := 0; i < count; i++ {
		interval := rng.ExpFloat64() * float64(editProbeMean)
		if math.IsNaN(interval) || math.IsInf(interval, 0) || interval >= float64(math.MaxInt64) {
			return editProbePlan{}, errors.New("poisson interval overflow")
		}
		delta := max(int64(1), int64(interval))
		if due > math.MaxInt64-delta {
			return editProbePlan{}, errors.New("poisson offered-plan duration overflow")
		}
		due += delta
		marker := fmt.Sprintf("GortexEditProbe%016xN%03d", uint64(seed), i)
		id := fmt.Sprintf("load-edit-%016x-%03d", uint64(seed), i)
		r := editRequest(c, marker)
		if tool == "edit.symbol" {
			r = editProbeSymbolRequest(c, marker)
		}
		if tool == "edit.write" || tool == "edit.batch" {
			if strings.Contains(c.EditInitialContent, marker) {
				return editProbePlan{}, errors.New("planned marker already exists in write fixture")
			}
			if tool == "edit.batch" {
				r, writeContent = editProbeBatchRequest(c, writeContent, marker, fmt.Sprintf("load-batch-%016x-%03d", uint64(seed), i))
			} else {
				r, writeContent = editProbeWriteRequest(c, writeContent, marker)
			}
		}
		beforeSymbol := ""
		if kind == "rename" {
			oldName := marker + "Before"
			before := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", oldName))
			if strings.Count(c.EditInitialContent, before) != 1 || strings.Contains(c.EditInitialContent, marker+"(") {
				return editProbePlan{}, errors.New("rename requires each exact original declaration and absent new declaration")
			}
			replacement := strings.TrimSpace(strings.ReplaceAll(c.EditTemplate, "{{marker}}", marker))
			beforeSymbol = c.File + "::" + oldName
			r = request{Tool: "edit", Operation: "symbol", Arguments: map[string]any{"operation": "symbol", "target": map[string]any{"symbol": beforeSymbol}, "match": before, "replacement": replacement}}
		}
		if tool != "edit.batch" {
			r.Arguments["options"] = map[string]any{"mutation_id": id}
		}
		hash, err := hashJSON(r)
		if err != nil {
			return editProbePlan{}, err
		}
		p.Steps = append(p.Steps, editProbeStep{Ordinal: i, ActivateNS: due - int64(editProbePeriod), DueNS: due, Marker: marker, MutationID: id, Request: r, RequestHash: hash, BeforeSymbol: beforeSymbol})
	}
	return p, nil
}

func (p editProbePlan) budget(deadline time.Duration, builds int) (time.Duration, error) {
	if len(p.Steps) == 0 || deadline <= 0 || builds < 1 || builds > 2 {
		return 0, errors.New("invalid finite edit-probe budget")
	}
	tail := p.Steps[len(p.Steps)-1].DueNS
	if int64(deadline) > math.MaxInt64/int64(builds) {
		return 0, errors.New("edit-probe deadline overflow")
	}
	drain := int64(deadline) * int64(builds)
	if tail > math.MaxInt64-drain {
		return 0, errors.New("edit-probe run budget overflow")
	}
	return time.Duration(tail + drain), nil
}

func editProbeOperations(plan editProbePlan, c Corpus) []request {
	var operations []request
	for _, step := range plan.Steps {
		operations = append(operations, step.Request)
		for _, oracle := range editProbeOracles(c, step) {
			operations = append(operations, oracle.call)
		}
	}
	return operations
}

// Each finite edit task waits against the same absolute origin. A blocked ACK
// cannot shift another offered arrival. Callbacks must honor the common context;
// all owned tasks join before the caller tears down sessions or the daemon.
func runEditProbeArrivals(ctx context.Context, p editProbePlan, origin time.Time, activate, dispatch func(context.Context, editProbeStep), sleep func(context.Context, time.Duration) error) error {
	var workers sync.WaitGroup
	for _, step := range p.Steps {
		workers.Go(func() {
			if sleep(ctx, time.Until(origin.Add(time.Duration(step.ActivateNS)))) != nil {
				return
			}
			activate(ctx, step)
			if sleep(ctx, time.Until(origin.Add(time.Duration(step.DueNS)))) != nil {
				return
			}
			dispatch(ctx, step)
		})
	}
	workers.Wait()
	return ctx.Err()
}
