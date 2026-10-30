package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"
)

type profile string

const (
	agentProfile      profile = "agent"
	saturationProfile profile = "saturation"
)

func parseProfile(value string) (profile, error) {
	p := profile(value)
	if p != agentProfile && p != saturationProfile {
		return "", fmt.Errorf("unknown profile %q (want agent or saturation)", value)
	}
	return p, nil
}

type pacing struct {
	profile profile
	random  *rand.Rand
}

func newPacing(p profile, seed uint64) *pacing {
	return &pacing{profile: p, random: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

func (p *pacing) think() time.Duration {
	if p.profile == saturationProfile {
		return 0
	}
	return 2*time.Second + time.Duration(p.random.Int64N(int64(3*time.Second)+1))
}

func (p *pacing) burst() int { return 3 + p.random.IntN(3) }

func busyBackoff(retry int) time.Duration {
	if retry >= 5 {
		return time.Second
	}
	return 50 * time.Millisecond * time.Duration(1<<max(0, retry))
}

type request struct {
	Tool      string         `json:"tool"`
	Operation string         `json:"operation"`
	Arguments map[string]any `json:"arguments"`
}

// Workload is the complete normalized offered load. Binary paths, private
// clone paths and revisions belong to the run identity, never the plan hash.
type Workload struct {
	Scenario         string    `json:"scenario"`
	Agents           int       `json:"agents"`
	EditAgents       int       `json:"edit_agents"`
	LinkedWorktrees  int       `json:"linked_worktrees"`
	DurationSec      float64   `json:"duration_seconds"`
	DeadlineSec      float64   `json:"product_deadline_seconds"`
	Seed             int64     `json:"seed"`
	Profile          string    `json:"profile"`
	Operations       []request `json:"operations"`
	CPUProfileSec    float64   `json:"cpu_profile_seconds"`
	WarmTimeoutSec   float64   `json:"warm_timeout_seconds"`
	StopTimeoutSec   float64   `json:"stop_timeout_seconds"`
	KeepState        bool      `json:"keep_state"`
	SessionModel     string    `json:"session_model"`
	SweepRepetitions int       `json:"sweep_repetitions"`
	SweepBudgetSec   float64   `json:"sweep_budget_seconds"`
}

func (r request) name() string {
	if r.Operation == "" {
		return r.Tool
	}
	return r.Tool + "." + r.Operation
}

func hashJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// These are logical, repo-relative targets. Expansion to a private clone's
// daemon repo prefix happens only at dispatch; the plan hash is identical per build.
func benchmarkPlan(c Corpus) ([]request, error) {
	if err := validateCorpus(c); err != nil {
		return nil, err
	}
	file := map[string]any{"file": c.File}
	symbol := map[string]any{"symbol": c.Symbol}
	requests := []request{
		{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": c.Query}},
		{Tool: "search", Operation: "text", Arguments: map[string]any{"operation": "text", "query": c.Query}},
		{Tool: "search", Operation: "files", Arguments: map[string]any{"operation": "files", "query": c.File}},
		{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": symbol}},
		{Tool: "read", Operation: "file", Arguments: map[string]any{"operation": "file", "target": file}},
		{Tool: "read", Operation: "editing_context", Arguments: map[string]any{"operation": "editing_context", "target": file}},
		{Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary", "target": file}},
		{Tool: "relations", Operation: "callers", Arguments: map[string]any{"operation": "callers", "target": symbol}},
		{Tool: "relations", Operation: "usages", Arguments: map[string]any{"operation": "usages", "target": symbol}},
		{Tool: "relations", Operation: "dependents", Arguments: map[string]any{"operation": "dependents", "target": symbol}},
		{Tool: "relations", Operation: "implementations", Arguments: map[string]any{"operation": "implementations", "target": symbol}},
		{Tool: "explore", Operation: "context", Arguments: map[string]any{"operation": "context", "task": c.Query}},
		{Tool: "explore", Operation: "outline", Arguments: map[string]any{"operation": "outline"}},
		{Tool: "explore", Operation: "localize", Arguments: map[string]any{"operation": "localize", "task": c.Query, "options": map[string]any{"new_user_task": true}}},
		{Tool: "change", Operation: "impact", Arguments: map[string]any{"operation": "impact", "target": symbol}},
		{Tool: "change", Operation: "verify", Arguments: map[string]any{"operation": "verify", "source": map[string]any{"changes": []map[string]any{{"symbol_id": c.Symbol, "new_signature": c.ProposedSignature}}}}},
		{Tool: "change", Operation: "tests", Arguments: map[string]any{"operation": "tests", "target": map[string]any{"symbols": []string{c.Symbol}}}},
		{Tool: "change", Operation: "detect", Arguments: map[string]any{"operation": "detect", "source": map[string]any{"scope": "unstaged"}}},
		{Tool: "trace", Operation: "call_chain", Arguments: map[string]any{"operation": "call_chain", "target": symbol}},
		{Tool: "analyze", Operation: "hotspots", Arguments: map[string]any{"kind": "hotspots"}},
		{Tool: "analyze", Operation: "dead_code", Arguments: map[string]any{"kind": "dead_code"}},
	}
	for i := range requests {
		requests[i].Arguments["output"] = map[string]any{"format": "json", "max_bytes": 0}
	}
	return requests, nil
}

// Sustained navigation excludes terminal localization and mutually-exclusive
// audits. The isolated sweep still measures the complete 21-operation set.
func readingPlan(c Corpus) ([]request, error) {
	all, err := benchmarkPlan(c)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{"search.symbols": true, "search.text": true, "read.source": true, "read.file": true, "read.editing_context": true, "read.summary": true, "relations.callers": true, "relations.usages": true, "explore.context": true, "change.impact": true}
	var selected []request
	for _, r := range all {
		if allowed[r.name()] {
			selected = append(selected, r)
		}
	}
	return selected, nil
}

var commitPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var labelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func validateCorpus(c Corpus) error {
	if !labelPattern.MatchString(c.Label) || c.Source == "" || !commitPattern.MatchString(c.Commit) {
		return errors.New("corpus requires a neutral label, source, and full 40-digit commit SHA")
	}
	// The manifest format is logical slash-relative on every host. IsAbs alone
	// accepts slash-rooted paths on Windows; VolumeName alone misses Windows
	// drives when the same manifest is validated on Unix. Reject both dialects
	// lexically, including drive-relative paths/alternate streams and traversal.
	if c.File == "." || !fs.ValidPath(c.File) || strings.ContainsAny(c.File, "\\:\x00") {
		return fmt.Errorf("corpus %s: file must be a canonical slash-separated repo-relative path", c.Label)
	}
	if c.Symbol == "" || strings.HasPrefix(c.Symbol, "ext::") || c.Query == "" {
		return fmt.Errorf("corpus %s: requires a source-backed symbol and query; ext:: targets are excluded", c.Label)
	}
	if c.ProposedSignature == "" {
		return fmt.Errorf("corpus %s: verify requires a language-valid proposed_signature for its source-backed symbol", c.Label)
	}
	return nil
}

// Editors already edit, observe indexed visibility, and then issue the reading
// burst. A zero pure-reader count must not quietly add another population to
// an editor-only workload. Other scenarios still require a reader population.
func validateAgentCounts(scenario string, readers, editors int) error {
	if readers < 0 || editors < 0 {
		return errors.New("reader/editor counts must be nonnegative")
	}
	switch scenario {
	case "edit", "many-edit":
		if editors < 1 {
			return errors.New("edit scenarios require at least one combined edit/readback agent")
		}
	case "all":
		if readers < 1 || editors < 1 {
			return errors.New("all scenarios require at least one pure reader and one combined edit/readback agent")
		}
	case "read", "many", "worktrees", "sweep", "cold", "restart":
		if readers < 1 {
			return errors.New("zero pure readers are supported only for edit/many-edit")
		}
	default:
		return fmt.Errorf("unknown scenario %q", scenario)
	}
	return nil
}
