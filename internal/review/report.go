package review

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graphpath"
)

// ReviewReport is the output of the hybrid review flow: a worst-of verdict over
// the merged deterministic + LLM findings, the per-file risk ranking, a one-line
// summary, and the run statistics. The findings are already line-anchored (the
// relocation phase ran) and deduplicated.
type ReviewReport struct {
	Verdict  Verdict     `json:"verdict"`
	Findings []Finding   `json:"findings"`
	FileRisk []FileRisk  `json:"file_risk"`
	Summary  string      `json:"summary"`
	Stats    ReviewStats `json:"stats"`
	// Depth is the adaptive review depth this changeset was classified into
	// (quick | standard | deep). It gates whether the LLM phase ran.
	Depth string `json:"depth"`
	// Cost is the per-review token + USD accounting. Set only when the
	// review ran through a usage-aware LLM seam (RunWithUsage); nil for
	// the deterministic-only or legacy Run path.
	Cost *CostBreakdown `json:"cost,omitempty"`
}

// FileRisk ranks one changed file by its impact-derived risk tier and the number
// of findings anchored to it. The tier is a blast-radius fact; Affected,
// Symbols, and Uncovered carry the evidence behind it so "CRITICAL" is
// explainable: how many symbols the change can reach, and how much of the
// change has covering tests. A fully covered file (Symbols > 0, Uncovered 0)
// contributes at most REVIEW to the verdict — blast radius alone does not
// block a well-tested change.
type FileRisk struct {
	File     string `json:"file"`
	Risk     string `json:"risk"`
	Findings int    `json:"findings"`
	// Affected is the widest blast radius among the file's changed symbols
	// (total transitively affected symbols from the impact analysis).
	Affected int `json:"affected,omitempty"`
	// Symbols counts the changed symbols in this file with impact data;
	// Uncovered counts how many of them have no covering test. A test
	// file's own symbols are never counted as uncovered — they are the
	// coverage.
	Symbols   int `json:"symbols,omitempty"`
	Uncovered int `json:"uncovered,omitempty"`
}

// ReviewStats records how the report was assembled: how many findings each
// source contributed and how many LLM candidates were dropped because no exact
// line could be grounded.
type ReviewStats struct {
	Rulepack     int            `json:"rulepack"`      // deterministic rule findings merged in
	LLM          int            `json:"llm"`           // LLM findings that anchored to a line
	Dropped      int            `json:"dropped"`       // LLM candidates dropped (unresolved anchor)
	Total        int            `json:"total"`         // findings in the report after dedup
	BySeverity   map[string]int `json:"by_severity"`   // severity histogram of the report
	Truncated    bool           `json:"truncated"`     // a budget bound dropped some candidates
	LLMRequested bool           `json:"llm_requested"` // the LLM phase was asked to run
	Gate         GateStats      `json:"gate"`          // confidence / severity / category / cap suppression summary
	Depth        string         `json:"depth"`         // adaptive review depth: quick | standard | deep
}

// computeVerdict is the worst-of verdict over the report's findings and per-file
// risk: any error/critical finding (or a critical-risk file) → BLOCK; any
// warning finding (or a high/medium-risk file) → REVIEW; otherwise APPROVE. It
// reuses the FromSeverity / FromRisk adapters so the ladder is never
// re-implemented here.
//
// Coverage tempers the risk side: a file whose changed symbols are all
// test-covered contributes at most REVIEW — blast radius says how far a
// regression could reach, covering tests say how likely it is to land
// unnoticed, and only the combination blocks.
func computeVerdict(findings []Finding, fileRisk []FileRisk) Verdict {
	worst := VerdictApprove
	for _, f := range findings {
		worst = worseVerdict(worst, FromSeverity(f.Severity))
	}
	for _, fr := range fileRisk {
		v := FromRisk(analysis.RiskLevel(fr.Risk))
		if fr.Symbols > 0 && fr.Uncovered == 0 && verdictRank(v) > verdictRank(VerdictReview) {
			v = VerdictReview
		}
		worst = worseVerdict(worst, v)
	}
	return worst
}

// verdictRank orders the verdicts so "worst-of" is a max over the rank.
func verdictRank(v Verdict) int {
	switch v {
	case VerdictBlock:
		return 2
	case VerdictReview:
		return 1
	default:
		return 0
	}
}

// worseVerdict returns the more severe of two verdicts.
func worseVerdict(a, b Verdict) Verdict {
	if verdictRank(b) > verdictRank(a) {
		return b
	}
	return a
}

// rankFileRisk derives the per-file risk ranking from the impact analysis and the
// findings anchored to each changed file. A file's risk tier is the impact tier
// of the worst changed symbol it contains; with no impact data it falls back to a
// finding-count heuristic via the same assessRisk ladder. The result is sorted
// worst-first, then by file for determinism.
//
// repoPrefix normalizes the two path vocabularies onto one key: changed-symbol
// paths come from graph nodes, which multi-repo daemons key as "<prefix>/<rel>",
// while findings and the diff's changed files are repo-relative. Without
// stripping the prefix off the graph-keyed side every file would surface twice —
// once with its real impact tier and once as a LOW diff-only row. The strip is
// applied per domain, never to every input: see the normalizers below.
//
// coverageKnown gates the coverage evidence: when the graph indexes no test
// symbols at all, "no covering test" is blindness, not a finding — the rows
// then carry no untested counts and the verdict keeps the conservative
// ladder. Test files are exempt from the coverage debt for the same reason:
// a test function needs no test of its own, so counting one as missing is a
// demand that can never be met.
func rankFileRisk(diff *analysis.DiffResult, impact map[string]*analysis.ImpactResult, findings []Finding, repoPrefix string, coverageKnown bool) []FileRisk {
	// Two vocabularies reach this function and they overlap, so each input is
	// normalized by its own domain instead of through one shared strip.
	// ChangedSymbol.FilePath is a graph node key; findings and ChangedFiles are
	// already repo-relative. Stripping the prefix off a repo-relative path that
	// legitimately begins with the prefix name rewrites it onto a different
	// file — `repo-a/pkg/widget.go` becomes `pkg/widget.go` — and attributes the
	// risk to that unchanged shadow.
	fromGraphKey := func(file string) string {
		// Normalize to the '/' comparison form BEFORE removing the prefix.
		// cleanPath ends in filepath.Clean, so on Windows the graph key
		// "repo-a/repo-a\widget.go" becomes "repo-a\repo-a\widget.go" and the
		// '/'-joined prefix no longer matches: the row keeps its prefix while
		// the repo-relative side produces a second row for the same file.
		file = graphpath.Norm(cleanPath(file))
		if repoPrefix != "" {
			file = strings.TrimPrefix(file, repoPrefix+"/")
		}
		return file
	}
	// Both domains leave here in the documented repo-relative '/' spelling —
	// the one the rule globs, the risk rows and the forge comment API speak.
	fromRepoRel := func(file string) string { return graphpath.Norm(cleanPath(file)) }

	byFile := map[string]string{}
	findingCount := map[string]int{}
	type coverage struct {
		affected  int
		symbols   int
		uncovered int
	}
	coverByFile := map[string]*coverage{}

	for _, f := range findings {
		if f.File != "" {
			findingCount[fromRepoRel(f.File)]++
		}
	}

	if diff != nil {
		for _, cs := range diff.ChangedSymbols {
			file := fromGraphKey(cs.FilePath)
			if file == "" {
				continue
			}
			risk := string(analysis.RiskLow)
			if impact != nil {
				ir := impact[cs.ID]
				if ir != nil && ir.Risk != "" {
					risk = string(ir.Risk)
				}
				// Blast-radius evidence (how far the worst changed symbol
				// reaches) is a graph fact and always recorded. Coverage
				// evidence is recorded only when the graph indexes tests
				// at all — an index that excludes test files must not
				// read as "untested".
				cov := coverByFile[file]
				if cov == nil {
					cov = &coverage{}
					coverByFile[file] = cov
				}
				if ir != nil && ir.TotalAffected > cov.affected {
					cov.affected = ir.TotalAffected
				}
				if coverageKnown {
					cov.symbols++
					// A test file's own symbols need no covering test of
					// their own, so "uncovered" is unsatisfiable for them:
					// every changeset touching tests pinned those files at
					// CRITICAL forever and blocked the branch with nothing
					// actionable to fix. The detectors already exclude test
					// files; the risk ranking now agrees. The row keeps its
					// blast-radius tier — only the coverage debt is dropped.
					if !analysis.IsTestFile(file) && (ir == nil || len(ir.TestFiles) == 0) {
						cov.uncovered++
					}
				}
			}
			byFile[file] = worseRisk(byFile[file], risk)
		}
		for _, file := range diff.ChangedFiles {
			file = fromRepoRel(file)
			if file == "" {
				continue
			}
			if _, ok := byFile[file]; !ok {
				byFile[file] = ""
			}
		}
	}

	// Files that carry findings but no diff/impact entry still get a row.
	for file := range findingCount {
		if _, ok := byFile[file]; !ok {
			byFile[file] = ""
		}
	}

	rows := make([]FileRisk, 0, len(byFile))
	for file, risk := range byFile {
		if risk == "" {
			risk = string(riskFromFindingCount(findingCount[file]))
		}
		row := FileRisk{File: file, Risk: risk, Findings: findingCount[file]}
		if cov := coverByFile[file]; cov != nil {
			row.Affected = cov.affected
			row.Symbols = cov.symbols
			row.Uncovered = cov.uncovered
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ri, rj := riskRank(rows[i].Risk), riskRank(rows[j].Risk)
		if ri != rj {
			return ri > rj
		}
		return rows[i].File < rows[j].File
	})
	return rows
}

// riskFromFindingCount maps the number of findings anchored to a file onto the
// impact-risk ladder so a file with no graph impact still ranks by how much the
// review flagged in it.
func riskFromFindingCount(n int) analysis.RiskLevel {
	switch {
	case n >= 5:
		return analysis.RiskHigh
	case n >= 2:
		return analysis.RiskMedium
	case n >= 1:
		return analysis.RiskLow
	default:
		return analysis.RiskLow
	}
}

func riskRank(r string) int {
	switch analysis.RiskLevel(r) {
	case analysis.RiskCritical:
		return 3
	case analysis.RiskHigh:
		return 2
	case analysis.RiskMedium:
		return 1
	default:
		return 0
	}
}

func worseRisk(a, b string) string {
	if riskRank(b) > riskRank(a) {
		return b
	}
	return a
}
