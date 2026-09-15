package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/viewmetrics"
)

// E2E matrix 2 — the edit taxonomy: every class of source change a view has to
// carry.
//
// Comments, presentation and directives; body, signature, export, import and
// configuration changes; added, deleted, renamed, mode-changed, symlinked and
// untracked paths; edit / undo / redo / commit; manifest-only changes. Each
// case applies one edit to an isolated daemon's checkout, waits for the view to
// carry it, and then compares that view against a FRESH ISOLATED INDEX of the
// very same tree — the gate-1 oracle:
//
//	"A composed/incremental view matches a fresh isolated index of the same
//	 target snapshot, configuration and producer set. Compare actual resolved
//	 targets, incoming/outgoing edges, locations, meaningful metadata,
//	 reference/unresolved facts, deletion absence, ownership, visibility,
//	 source/text/search and completeness claims. Equality of unresolved
//	 placeholders or node counts is insufficient."
//
// The comparison is therefore never a node count. It is the normalized equality
// of five product-surface projections per case, each carrying a named part of
// that clause:
//
//	read.summary   — nodes with kind, language, signature, visibility, doc and
//	                 line span, plus every edge of the file with its endpoints
//	                 and line: locations, metadata, ownership, visibility,
//	                 outgoing and same-file incoming edges.
//	relations.usages — incoming edges from anywhere in the corpus.
//	search.symbols — the resolved target, its repo prefix and its file; the
//	                 same probe with no hit is the deletion-absence evidence.
//	read.source    — the served bytes and the span they were served from.
//	search.text    — the text lane, which is a different corpus from the
//	                 symbol lane and can disagree with it.
//
// Two axes are deliberately NOT in the asserted equality, and both are recorded
// instead:
//
//   - Edge provenance (origin / tier / confidence_label). The language-server
//     enrichment pass upgrades ast_resolved edges to lsp_resolved
//     asynchronously and independently in each process, so two correct indexes
//     of the same tree legitimately differ in provenance at any instant. It is
//     compared separately and reported as an observation; exact restub and
//     provenance behaviour is matrix 3's subject.
//   - etag / fetched_at, which are per-process cache identity, not content.
//
// Opt-in through GX_SUSTAINED_IO_TEST_BINARY (see e2e_matrix_noop_test.go), private daemon,
// private store, private git. GX_E2E_MATRIX_ORACLE=0 records every fresh-index
// comparison as a named skip for a fast structural smoke run.

// The oracle re-reads both sides when they disagree: an enrichment pass that
// has not finished on one of them is a converging difference, not a defect, and
// a single sample would report it as one. Three attempts with a minute between
// them bound the cost — each attempt builds another fresh isolated index — while
// giving the asynchronous lanes (language server, go/types semantics) a real
// chance to land on both sides. The budget is a knob because "how long did you
// wait" is part of what a reported difference means.
const (
	e2eMatrixOracleAttempts       = 3
	e2eMatrixDefaultOracleBackoff = time.Minute
)

// e2eMatrixOracleBackoff is how long the oracle waits between two disagreeing
// samples. GX_E2E_MATRIX_ORACLE_BACKOFF overrides it; the value in force is
// reported on every row that reports a difference.
func e2eMatrixOracleBackoff(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv("GX_E2E_MATRIX_ORACLE_BACKOFF")
	if raw == "" {
		return e2eMatrixDefaultOracleBackoff
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < 0 || parsed > 10*time.Minute {
		t.Fatalf("GX_E2E_MATRIX_ORACLE_BACKOFF=%q is not a duration in [0,10m]", raw)
	}
	return parsed
}

// ------------------------------------------------------------- projections ---

// e2eMatrixProbe is one product-surface question asked of both indexes.
type e2eMatrixProbe struct {
	// Key names the probe in the comparison report.
	Key string
	// Clause is the gate-1 vocabulary this probe evidences.
	Clause string
	// Facade and Payload are the CLI call: `gortex call <facade> --json …`.
	Facade  string
	Payload map[string]any
	// Name, when set, reduces the answer to the rows that ARE this symbol
	// (see e2eMatrixReduceToName). Only the symbol-search probe sets it.
	Name string
}

// e2eMatrixReduceToName keeps the rows of a search answer that are the named symbol
// and returns them as the whole answer, dropping the page around them.
//
// Why the page itself is not the comparison: `search symbols` answers a RANKED
// PAGE. The generated corpus carries one revision probe per file
// (GxProbe00000Rev0, GxProbe00013Rev0, GxProbe00021Rev0 …), so a query for one
// of them matches dozens of identically-shaped names and the ten rows that come
// back are a tie-break among equals — two correct indexes of the same tree
// legitimately return different members of that tie, at different ranks. That
// was observed directly: a body_change case failed on a symbols probe whose
// only difference was three unrelated probe names on one side and three others
// on the other, every one of them from a file the case never touched.
//
// What gate 1 asks of this probe is the actual resolved TARGET of a name, and
// whether a name that should be gone still answers. Both survive the reduction:
// the kept rows carry the id, the file, the kind, the visibility, the line and
// the metadata of the symbol, and a name that is gone reduces to no rows at all.
// What is deliberately NOT compared is the ranking of unrelated symbols, which
// is not a property of the tree.
func e2eMatrixReduceToName(value any, name string) any {
	// Non-nil: "the name answers with nothing" has to render as an empty list,
	// not as null, so an absent symbol and an unanswered probe stay different
	// facts in the comparison.
	rows := []any{}
	var walk func(any)
	walk = func(node any) {
		switch node := node.(type) {
		case map[string]any:
			if got, ok := node["name"].(string); ok && got == name {
				if _, hasID := node["id"]; hasID {
					rows = append(rows, node)
					return
				}
			}
			keys := make([]string, 0, len(node))
			for key := range node {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				walk(node[key])
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(value)
	sort.Slice(rows, func(i, j int) bool { return e2eMatrixCanonical(rows[i]) < e2eMatrixCanonical(rows[j]) })
	return rows
}

// e2eMatrixVolatileKeys are per-process cache identity, never content.
func e2eMatrixVolatileKeys() map[string]bool {
	return map[string]bool{"etag": true, "fetched_at": true}
}

// e2eMatrixProvenanceKeys are the asynchronous enrichment axis. They are dropped
// from the asserted structural comparison and compared separately.
func e2eMatrixProvenanceKeys() map[string]bool {
	return map[string]bool{"origin": true, "tier": true, "confidence_label": true}
}

// e2eMatrixSemanticMetadataKeys is a MEASURED GAP of this branch, not a normalization
// convenience.
//
// A file this daemon rebuilt after an edit carries no go/types semantic
// metadata, while a fresh isolated index of the same tree does. Observed
// directly on one live daemon at one instant: every node of a rebuilt file
// (p000/file00000.go, committed ~10 minutes earlier) had
// meta.semantic_source / meta.semantic_type / meta.return_type absent, while
// every node of an untouched file in the same graph (p002/file00010.go) still
// carried semantic_source=go-types. It is not a convergence artifact: it
// survives every sample this oracle takes, and it is confined to these three
// keys.
//
// A case whose ONLY difference from the fresh index is these keys is therefore
// filed as a named skip pointing at what OWNS the gap — see
// e2eMatrixKnownGapReason for what that is, and what does not. Any
// difference outside these keys stays a hard gate-1 failure, and the full
// difference is recorded in the row either way — a skipped row is a reported
// gap, never a silent pass.
func e2eMatrixSemanticMetadataKeys() map[string]bool {
	return map[string]bool{"semantic_source": true, "semantic_type": true, "return_type": true}
}

// e2eMatrixKnownGapReason is the sentence a skipped row carries.
//
// The attribution was corrected after the adversarial review checked it against
// the ledger. The owner is the in-memory incremental-build route's own declared
// limitation, and the mechanism fits exactly: go/types needs a package's other
// files to type-check one of them, and that route runs enrichment "after the
// pass, when the read-only half is already gone", so the closure a rebuilt file
// needs is no longer there — which is why the rebuilt file loses the metadata
// while an untouched file in the same graph keeps it.
//
// The owners the first draft cited do NOT own it and are named here so the
// ledger cannot inherit the mistake: the semantic-enrichment pass and the
// enrichment-census work are both `wired` (the latter absorbed into the
// incremental-build route and committed as 2b76f61a), the census having
// discharged its second clause. The blame / coverage input side channel is
// genuinely unstarted, but it is an input channel, not go/types.
const e2eMatrixKnownGapReason = "the branch does not carry go/types semantic metadata (meta.semantic_source / semantic_type / return_type) onto a rebuilt file: the incremental view lacks what a fresh isolated index of the same tree has, while an untouched file in the same graph still carries it. Owner: the in-memory incremental-build route's declared limitation — 'semantic enrichment now sees the change set, not the closure' — so the go/types pass over a rebuilt file no longer has the package closure it needs. The semantic-enrichment pass and the enrichment census are 'wired' and do NOT own this; the blame/coverage input channel is open but is an input channel"

func e2eMatrixMergedKeys(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, set := range sets {
		for key := range set {
			out[key] = true
		}
	}
	return out
}

// e2eMatrixNormalize makes one answer comparable across two daemons: the answering
// daemon's own root becomes <ROOT>, the named keys are dropped, and every list
// is ordered by its canonical rendering so ranking cannot masquerade as a
// content difference.
func e2eMatrixNormalize(value any, root string, drop map[string]bool) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			if drop[key] {
				continue
			}
			out[key] = e2eMatrixNormalize(child, root, drop)
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, child := range value {
			out = append(out, e2eMatrixNormalize(child, root, drop))
		}
		sort.Slice(out, func(i, j int) bool { return e2eMatrixCanonical(out[i]) < e2eMatrixCanonical(out[j]) })
		return out
	case string:
		if root != "" {
			return strings.ReplaceAll(value, root, "<ROOT>")
		}
		return value
	default:
		return value
	}
}

// e2eMatrixCanonical renders a normalized value deterministically. encoding/json
// sorts map keys, so this is a stable identity for comparison and for diffing.
func e2eMatrixCanonical(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(raw)
}

// e2eMatrixDifference is one probe whose two answers disagree.
type e2eMatrixDifference struct {
	Probe       string `json:"probe"`
	Clause      string `json:"gate1_clause"`
	Incremental string `json:"incremental"`
	Fresh       string `json:"fresh_isolated_index"`
}

// e2eMatrixCompare reports the probes whose normalized answers differ. It is pure so
// the comparison rule is tested without a daemon.
func e2eMatrixCompare(incremental, fresh map[string]string, probes []e2eMatrixProbe) []e2eMatrixDifference {
	var differences []e2eMatrixDifference
	for _, probe := range probes {
		left, hasLeft := incremental[probe.Key]
		right, hasRight := fresh[probe.Key]
		switch {
		case !hasLeft || !hasRight:
			differences = append(differences, e2eMatrixDifference{Probe: probe.Key, Clause: probe.Clause,
				Incremental: e2eMatrixMissing(hasLeft, left), Fresh: e2eMatrixMissing(hasRight, right)})
		case left != right:
			differences = append(differences, e2eMatrixDifference{Probe: probe.Key, Clause: probe.Clause,
				Incremental: left, Fresh: right})
		}
	}
	return differences
}

// e2eMatrixTrivialAnswer reports whether a projected answer carries no evidence of
// its own — an empty reduction, or a transport/decode failure recorded as the
// answer.
//
// Why it matters: e2eMatrixProject stores "call error: …" as the answer so that two
// daemons which both refuse a probe agree rather than the harness inventing a
// difference. That is the right rule for a probe, and the wrong rule for a
// CASE: a case whose whole comparison is two error strings, or two empty row
// lists, reports "N probes equal" while neither index answered anything. The
// review found file_deleted resting on exactly one such probe.
func e2eMatrixTrivialAnswer(value string) bool {
	trimmed := strings.TrimSpace(value)
	switch trimmed {
	case "", "[]", "{}", "null":
		return true
	}
	for _, prefix := range []string{"call error: ", "decode error: ", "marshal error: "} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// e2eMatrixSubstantiveProbes counts the probes BOTH indexes answered with something,
// and names the ones that carry no evidence. A comparison with no substantive
// probe is refused by e2eMatrixGateOneVerdict.
func e2eMatrixSubstantiveProbes(incremental, fresh map[string]string, probes []e2eMatrixProbe) (substantive int, empty []string) {
	for _, probe := range probes {
		left, hasLeft := incremental[probe.Key]
		right, hasRight := fresh[probe.Key]
		switch {
		case !hasLeft || !hasRight:
			// A probe one side never produced is a difference, not agreement;
			// e2eMatrixCompare reports it and it is neither substantive nor empty.
		case e2eMatrixTrivialAnswer(left) && e2eMatrixTrivialAnswer(right):
			empty = append(empty, probe.Key)
		default:
			substantive++
		}
	}
	sort.Strings(empty)
	return substantive, empty
}

// e2eMatrixOracleResult is what one oracle round produced: the surviving
// differences, how many of them are not the named gap, how many probes differ
// once provenance is included, and how much of the comparison was substantive.
type e2eMatrixOracleResult struct {
	Differences []e2eMatrixDifference
	Residual    []e2eMatrixDifference
	Provenance  int
	Substantive int
	Empty       []string
}

// e2eMatrixOracleInput is everything the gate-1 decision is made from.
type e2eMatrixOracleInput struct {
	Probes      int
	Substantive int
	Empty       []string
	Differences []e2eMatrixDifference
	Residual    []e2eMatrixDifference
	Attempts    int
	Backoff     time.Duration
}

// e2eMatrixGateOneVerdict is matrix 2's decision, separated from the daemon so the
// rule that turns a surviving difference into a FAIL — rather than into the
// named-gap SKIP beside it — is pinned by an ordinary unit test. The review
// found that converting this branch to a skip left the whole suite green.
func e2eMatrixGateOneVerdict(in e2eMatrixOracleInput) e2eMatrixVerdict {
	switch {
	case in.Probes == 0:
		return e2eMatrixVerdict{Status: e2eMatrixStatusFail,
			Failures: []string{"gate1: the case declared no projection to compare"}}
	case in.Substantive == 0:
		return e2eMatrixVerdict{Status: e2eMatrixStatusFail, Failures: []string{fmt.Sprintf(
			"gate1: the comparison rests on no substantive answer — all %d probe(s) are empty or were refused by both daemons (%s), and two indexes that answered nothing agree for free",
			in.Probes, strings.Join(in.Empty, " "))}}
	case len(in.Differences) == 0:
		return e2eMatrixVerdict{Status: e2eMatrixStatusPass, Recorded: []string{fmt.Sprintf(
			"gate1 oracle: %d probes equal (%d substantive, %d carrying no evidence)",
			in.Probes, in.Substantive, len(in.Empty))}}
	case len(in.Residual) == 0:
		return e2eMatrixVerdict{Status: e2eMatrixStatusSkip, Recorded: []string{fmt.Sprintf(
			"gate1: %d probe(s) differ (%d substantive probe(s) compared) and every difference is %s",
			len(in.Differences), in.Substantive, e2eMatrixKnownGapReason)}}
	default:
		rendered := make([]string, 0, len(in.Residual))
		for _, difference := range in.Residual {
			rendered = append(rendered, fmt.Sprintf("%s [%s]: %s", difference.Probe, difference.Clause,
				e2eMatrixDescribeDifference(difference.Incremental, difference.Fresh)))
		}
		return e2eMatrixVerdict{Status: e2eMatrixStatusFail, Failures: []string{fmt.Sprintf(
			"gate1: the incremental view still differs from a fresh isolated index of the same tree after %d samples %s apart, beyond the known semantic-metadata gap: %s",
			in.Attempts, in.Backoff, strings.Join(rendered, " ;; "))}}
	}
}

func e2eMatrixMissing(present bool, value string) string {
	if present {
		return value
	}
	return "<probe not answered>"
}

// e2eMatrixDescribeDifference says WHAT differs between two canonical answers
// instead of printing both of them. Two 4 KB edge lists that differ by one edge
// are unreadable side by side and unactionable truncated; the set difference is
// one line and names the edge.
func e2eMatrixDescribeDifference(left, right string) string {
	var leftValue, rightValue any
	if json.Unmarshal([]byte(left), &leftValue) != nil || json.Unmarshal([]byte(right), &rightValue) != nil {
		return fmt.Sprintf("incremental=%s fresh=%s", e2eMatrixTruncate(left, 300), e2eMatrixTruncate(right, 300))
	}
	parts := e2eMatrixDescribeValue("", leftValue, rightValue)
	if len(parts) == 0 {
		return "the two answers render differently but compare equal"
	}
	return strings.Join(parts, "; ")
}

// e2eMatrixDescribeValue walks two decoded answers in parallel and names the first
// level at which they part company: a missing key, a scalar that moved, or the
// entries that exist on only one side of a list.
func e2eMatrixDescribeValue(path string, left, right any) []string {
	if e2eMatrixCanonical(left) == e2eMatrixCanonical(right) {
		return nil
	}
	name := path
	if name == "" {
		name = "."
	}
	leftMap, leftIsMap := left.(map[string]any)
	rightMap, rightIsMap := right.(map[string]any)
	if leftIsMap && rightIsMap {
		keys := map[string]bool{}
		for key := range leftMap {
			keys[key] = true
		}
		for key := range rightMap {
			keys[key] = true
		}
		ordered := make([]string, 0, len(keys))
		for key := range keys {
			ordered = append(ordered, key)
		}
		sort.Strings(ordered)
		var parts []string
		for _, key := range ordered {
			leftChild, hasLeft := leftMap[key]
			rightChild, hasRight := rightMap[key]
			switch {
			case !hasLeft:
				parts = append(parts, fmt.Sprintf("%s.%s only in fresh: %s", name, key, e2eMatrixTruncate(e2eMatrixCanonical(rightChild), 200)))
			case !hasRight:
				parts = append(parts, fmt.Sprintf("%s.%s only in incremental: %s", name, key, e2eMatrixTruncate(e2eMatrixCanonical(leftChild), 200)))
			default:
				parts = append(parts, e2eMatrixDescribeValue(path+"."+key, leftChild, rightChild)...)
			}
		}
		return parts
	}
	leftList, leftIsList := left.([]any)
	rightList, rightIsList := right.([]any)
	if leftIsList && rightIsList {
		onlyLeft := e2eMatrixListMinus(leftList, rightList)
		onlyRight := e2eMatrixListMinus(rightList, leftList)
		var parts []string
		if len(onlyLeft) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d entr(ies) only in incremental: %s", name, len(onlyLeft), e2eMatrixTruncate(strings.Join(onlyLeft, " "), 600)))
		}
		if len(onlyRight) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d entr(ies) only in the fresh index: %s", name, len(onlyRight), e2eMatrixTruncate(strings.Join(onlyRight, " "), 600)))
		}
		if len(parts) == 0 {
			parts = append(parts, fmt.Sprintf("%s: same entries in a different multiplicity (%d vs %d)", name, len(leftList), len(rightList)))
		}
		return parts
	}
	return []string{fmt.Sprintf("%s: incremental=%s fresh=%s", name,
		e2eMatrixTruncate(e2eMatrixCanonical(left), 200), e2eMatrixTruncate(e2eMatrixCanonical(right), 200))}
}

// e2eMatrixListMinus is the multiset difference of two decoded lists, rendered.
func e2eMatrixListMinus(left, right []any) []string {
	counts := map[string]int{}
	for _, item := range right {
		counts[e2eMatrixCanonical(item)]++
	}
	var only []string
	for _, item := range left {
		key := e2eMatrixCanonical(item)
		if counts[key] > 0 {
			counts[key]--
			continue
		}
		only = append(only, key)
	}
	return only
}

// --------------------------------------------------------------- the cases ---

// e2eMatrixSighting is one "this name must (not) answer out of this file" fact.
type e2eMatrixSighting struct {
	Name string
	Rel  string // repo-relative, slash-separated
}

// e2eMatrixEffect is what a case changed and what has to be probed afterwards.
type e2eMatrixEffect struct {
	Files   []string            // repo-relative files to project (must exist after the case)
	Symbols []e2eMatrixSighting // symbols to project through usages/source
	Present []e2eMatrixSighting // names that must answer from their file
	Absent  []e2eMatrixSighting // names that must NOT answer (deletion absence)
	// Observe are names the oracle projects without the case asserting
	// anything about them. They are how a case asks a question whose answer
	// is a product behaviour rather than a gate: "is a gitignored file
	// indexed at all?" is not something gate 1 decides — gate 1 decides
	// whether the composed view and a fresh isolated index of the same tree
	// AGREE about it. The case records which way the branch answered, and the
	// oracle still compares the name on both sides.
	Observe []e2eMatrixSighting
	Texts   []string // literals for the text lane
	Note    string
}

// e2eMatrixEditCase is one row of matrix 2.
type e2eMatrixEditCase struct {
	Name  string
	Gate  string
	Apply func(h *e2eMatrixEditHarness) e2eMatrixEffect
}

// e2eMatrixEditCaseNames is the declared vocabulary for this matrix. The case
// table is pinned to it so a class cannot quietly leave the taxonomy.
func e2eMatrixEditCaseNames() []string {
	return []string{
		"comment_only",
		"presentation_only",
		"directive_added",
		"body_change",
		"signature_change",
		"export_change",
		"import_change",
		"file_added",
		"file_deleted",
		"file_renamed",
		"mode_change",
		"symlink_added",
		"untracked_path",
		"edit_undo_redo",
		"commit_unchanged_content",
		"manifest_only_change",
		"configuration_change",
	}
}

// e2eMatrixEditCases is matrix 2, in the declared order of the taxonomy.
func e2eMatrixEditCases() []e2eMatrixEditCase {
	return []e2eMatrixEditCase{
		{
			Name: "comment_only", Gate: "gate1+gate3",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel := h.rel(0)
				h.rewrite(rel, func(source string) string {
					return "// e2eMatrix: a comment-only change carries no declaration.\n" + source
				})
				h.commit("comment only")
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{h.leaf(0), rel}},
					Present: []e2eMatrixSighting{{h.probe(0), rel}},
					Texts:   []string{"a comment-only change carries no declaration"},
					Note:    "a comment line added above the package clause",
				}
			},
		},
		{
			Name: "presentation_only", Gate: "gate1+gate3",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel := h.rel(1)
				h.rewrite(rel, func(source string) string {
					// One more blank line wherever there was one: every byte
					// offset and every line number below the first paragraph
					// moves, and no declaration does.
					return strings.ReplaceAll(source, "\n\n", "\n\n\n")
				})
				h.commit("presentation only")
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{h.leaf(1), rel}},
					Present: []e2eMatrixSighting{{h.probe(1), rel}},
					Note:    "vertical whitespace only; every declaration keeps its name and its order",
				}
			},
		},
		{
			Name: "directive_added", Gate: "gate1+gate3",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel := h.rel(2)
				h.rewrite(rel, func(source string) string {
					return strings.Replace(source, "\nfunc ", "\n//go:generate echo e2eMatrix\nfunc ", 1)
				})
				h.commit("directive added")
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{h.leaf(2), rel}},
					Present: []e2eMatrixSighting{{h.probe(2), rel}},
					Texts:   []string{"go:generate echo e2eMatrix"},
					Note:    "a //go:generate directive above the first function",
				}
			},
		},
		{
			Name: "body_change", Gate: "gate1+gate4",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel := h.rel(3)
				index := h.index(3)
				h.rewriteFile(rel, func(source string) string {
					edited, err := sustainedIOEditFileSource(source, index, 1)
					if err != nil {
						h.t.Fatal(err)
					}
					return edited
				})
				h.commit("body change")
				h.revision[index] = 1
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{h.leaf(3), rel}},
					Present: []e2eMatrixSighting{{sustainedIOProbeName(index, 1), rel}},
					Absent:  []e2eMatrixSighting{{sustainedIOProbeName(index, 0), rel}},
					Note:    "one function body and the revision stub",
				}
			},
		},
		{
			Name: "signature_change", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel, index := h.rel(4), h.index(4)
				leaf := h.leaf(4)
				h.rewrite(rel, func(source string) string {
					// The leaf gains a parameter; its same-file callers are
					// updated, so the edit is a real signature change with a
					// real call-site rewrite rather than a broken file.
					source = strings.Replace(source,
						fmt.Sprintf("func %s() int { return ", leaf),
						fmt.Sprintf("func %s(e2eMatrixBias int) int { return e2eMatrixBias + ", leaf), 1)
					return strings.ReplaceAll(source, leaf+"()", leaf+"(1)")
				})
				h.commit("signature change")
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{leaf, rel}},
					Present: []e2eMatrixSighting{{sustainedIOProbeName(index, h.revision[index]), rel}, {leaf, rel}},
					Texts:   []string{"e2eMatrixBias"},
					Note:    "a parameter added to the leaf function and every same-file call site rewritten",
				}
			},
		},
		{
			Name: "export_change", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel := h.rel(5)
				old := h.leaf(5)
				renamed := "e2eMatrixUnexported" + strings.TrimPrefix(old, "Fn")
				h.rewrite(rel, func(source string) string { return strings.ReplaceAll(source, old, renamed) })
				h.commit("export change")
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{renamed, rel}},
					Present: []e2eMatrixSighting{{renamed, rel}},
					Absent:  []e2eMatrixSighting{{old, rel}},
					Note:    "an exported leaf made unexported; visibility and every same-file edge move with it",
				}
			},
		},
		{
			Name: "import_change", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel := h.rel(6)
				// A new file in package p000 that imports p001 and calls into
				// it: a new cross-package import and a new resolved edge.
				importer := "p000/e2eMatrix_import.go"
				h.write(importer, "package p000\n\nimport e2eMatrixcross \""+sustainedIOModule+"/p001\"\n\n"+
					"// GxEditsImportedCaller is the import-change probe.\nfunc GxEditsImportedCaller() int { return e2eMatrixcross."+h.leafFor(1)+"() }\n")
				h.commit("import change")
				return e2eMatrixEffect{
					Files: []string{importer, "p001/" + filepath.Base(h.relFor(1))}, Symbols: []e2eMatrixSighting{{"GxEditsImportedCaller", importer}},
					Present: []e2eMatrixSighting{{"GxEditsImportedCaller", importer}, {h.probe(6), rel}},
					Texts:   []string{"GxEditsImportedCaller"},
					Note:    "a new file adding a cross-package import and a resolved cross-package call",
				}
			},
		},
		{
			Name: "file_added", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				added := "p002/e2eMatrix_added.go"
				h.write(added, "package p002\n\n// GxEditsAddedSymbol is the added-file probe.\nfunc GxEditsAddedSymbol() int { return 8005 }\n")
				h.commit("file added")
				return e2eMatrixEffect{
					Files: []string{added}, Symbols: []e2eMatrixSighting{{"GxEditsAddedSymbol", added}},
					Present: []e2eMatrixSighting{{"GxEditsAddedSymbol", added}},
					Texts:   []string{"GxEditsAddedSymbol"},
					Note:    "one new file in an existing package",
				}
			},
		},
		{
			Name: "file_deleted", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				deleted := "p002/e2eMatrix_added.go"
				h.git("rm", "-q", "-f", filepath.FromSlash(deleted))
				h.commit("file deleted")
				// The deletion probe reduces to an EMPTY row list on both
				// sides, and two empty lists agree for free: on its own this
				// case would report "1 probe equal" without either index
				// having answered anything. The review found exactly that. So
				// the effect also projects a file this case does not touch
				// (slot 10, used by no other case), which gives the comparison
				// at least one substantive probe — a summary, a usages set and
				// a source span that are non-empty on both sides. run() now
				// refuses a comparison that has none.
				control := h.rel(10)
				return e2eMatrixEffect{
					Files:   []string{control},
					Symbols: []e2eMatrixSighting{{h.leaf(10), control}},
					Present: []e2eMatrixSighting{{h.probe(10), control}},
					Absent:  []e2eMatrixSighting{{"GxEditsAddedSymbol", deleted}},
					Note:    "the added file removed again; deletion absence is the assertion, beside an untouched control file so the comparison is not two empty answers",
				}
			},
		},
		{
			Name: "file_renamed", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				from, to := "p000/e2eMatrix_import.go", "p000/e2eMatrix_renamed.go"
				h.git("mv", filepath.FromSlash(from), filepath.FromSlash(to))
				h.commit("file renamed")
				return e2eMatrixEffect{
					Files: []string{to}, Symbols: []e2eMatrixSighting{{"GxEditsImportedCaller", to}},
					Present: []e2eMatrixSighting{{"GxEditsImportedCaller", to}},
					Absent:  []e2eMatrixSighting{{"GxEditsImportedCaller", from}},
					Note:    "the same declaration under a new path: the old location must be gone",
				}
			},
		},
		{
			Name: "mode_change", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel := h.rel(7)
				path := h.path(rel)
				if err := os.Chmod(path, 0o700); err != nil {
					h.t.Fatal(err)
				}
				h.commit("mode change")
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{h.leaf(7), rel}},
					Present: []e2eMatrixSighting{{h.probe(7), rel}},
					Note:    "the file mode changed and not one byte of content",
				}
			},
		},
		{
			Name: "symlink_added", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel := h.rel(8)
				// The link deliberately does not end in .go: a second path to
				// the same Go file inside the same package would duplicate
				// every declaration in it, and the case would then be about
				// duplicate declarations rather than about symlinks.
				link := "p000/e2eMatrix_link.txt"
				linkPath := h.path(link)
				_ = os.Remove(linkPath)
				target, err := filepath.Rel(filepath.Dir(linkPath), h.path(rel))
				if err != nil {
					h.t.Fatal(err)
				}
				if err := os.Symlink(target, linkPath); err != nil {
					h.t.Skipf("this host refuses symlinks in the fixture: %v", err)
				}
				h.commit("symlink added")
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{h.leaf(8), rel}},
					Present: []e2eMatrixSighting{{h.probe(8), rel}},
					Note:    "a symlink pointing at an indexed file; both indexes must reach the same verdict about it",
				}
			},
		},
		{
			Name: "untracked_path", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				ignored := "e2eMatrix_ignored/ignored.go"
				// Order matters for what this case can be said to show, and
				// the review corrected the first description of it: the
				// .gitignore is written BEFORE the file, so the file is
				// ignored from the moment it appears. Nothing is "retained"
				// here — the question is whether a path that was ignored
				// before it ever existed is indexed, and whether the composed
				// view and a fresh isolated index of the same tree AGREE about
				// it. (The "already-indexed content withdrawn by a newly
				// committed exclusion" shape is configuration_change, below.)
				h.write(".gitignore", "e2eMatrix_ignored/\n")
				h.write(ignored, "package e2eMatrixignored\n\n// GxEditsIgnoredSymbol is the untracked-path probe.\nfunc GxEditsIgnoredSymbol() int { return 1 }\n")
				h.commit("gitignore the untracked path")
				h.settle()
				// Whether an ignored, never-committed file is indexed at all
				// is a product question, not a gate: what gate 1 decides is
				// whether the composed view and a fresh isolated index of the
				// same tree agree about it. Measured on this branch: the file
				// IS indexed and served ("the name is STILL served out of that
				// file (found=true, repo_prefix=\"issue767\")"), so asserting
				// its absence here would report a product opinion as a
				// composition defect. The answer is recorded and compared.
				//
				// And the two indexes DISAGREE, which is a gate-1 divergence
				// and not a product opinion. Measured at the final source, on
				// three samples a minute apart: GxEditsIgnoredSymbol is present in
				// the incrementally maintained view and absent from a fresh
				// isolated index of the SAME tree — same bytes on disk, same
				// .gitignore, and the fresh fixture leaves the file untracked
				// too (newIssue767FixtureWithCorpus stages with `git add .`,
				// which honours the ignore). The cold path applies the ignore
				// to a file that is present when it indexes; the incremental
				// path admits the same file when the watcher sees it appear.
				// The row stays RED on purpose: it is the divergence, not the
				// policy, that this gate exists to catch.
				h.observe("the ignored, never-committed file", "GxEditsIgnoredSymbol", ignored)
				return e2eMatrixEffect{
					Files:   []string{h.rel(0)},
					Observe: []e2eMatrixSighting{{"GxEditsIgnoredSymbol", ignored}},
					Note:    "a path ignored before it ever existed, present in the working tree",
				}
			},
		},
		{
			Name: "edit_undo_redo", Gate: "gate4",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				rel, index := h.rel(9), h.index(9)
				original := h.read(rel)
				// Edit, undo to the exact original bytes, then redo. Every
				// step is dirty — nothing is committed — so the case is about
				// reusing work across a round trip, not about publishing.
				//
				// The undo and the redo each get their own measurement
				// window, because the gate-4 claim is about THOSE steps.
				//
				// The first edit is a real change and is expected to build, so
				// it doubles as the CALIBRATION of the reuse instrument: it is
				// the one step in this case that must move the build series if
				// the series moves at all here. Without it, "built_dirty did
				// not move" is unfalsifiable — the adversarial review measured
				// that no dirty build in either matrix ever moved a
				// coordinator_cycle series, so the reading carried no
				// information while the row claimed gate 4 confirmed. With it,
				// a silent calibration makes the undo/redo readings
				// NOT MEASURABLE and the row a named skip
				// (e2eMatrixReuseVerdict, and the harness's unmeasurable list).
				h.rebaseline()
				h.rewriteFile(rel, func(string) string { return h.edited(original, index, 1) })
				h.waitPresent(sustainedIOProbeName(index, 1), rel)
				h.settle()
				calibration, calibrationErr := h.deltaSince()
				if calibrationErr != nil {
					h.note("the gate-4 reuse instrument could not be calibrated: " + calibrationErr.Error())
				} else {
					h.note("gate-4 calibration — a real dirty rebuild moved [" +
						strings.Join(e2eMatrixMovedSeries(calibration, e2eMatrixDirtyBuildSeries()), " ") + "]")
				}

				h.rebaseline()
				h.rewriteFile(rel, func(string) string { return original })
				h.waitPresent(sustainedIOProbeName(index, 0), rel)
				h.settle()
				undo := h.reuseSince("undo to the byte-identical original", calibration, e2eMatrixDirtyBuildSeries())

				h.rebaseline()
				h.rewriteFile(rel, func(string) string { return h.edited(original, index, 1) })
				h.waitPresent(sustainedIOProbeName(index, 1), rel)
				h.settle()
				redo := h.reuseSince("redo of the already-seen edit", calibration, e2eMatrixDirtyBuildSeries())

				h.revision[index] = 1
				return e2eMatrixEffect{
					Files: []string{rel}, Symbols: []e2eMatrixSighting{{h.leaf(9), rel}},
					Present: []e2eMatrixSighting{{sustainedIOProbeName(index, 1), rel}},
					Absent:  []e2eMatrixSighting{{sustainedIOProbeName(index, 0), rel}},
					Note:    "dirty edit, undo to the byte-identical original, redo | " + undo + " | " + redo,
				}
			},
		},
		{
			Name: "commit_unchanged_content", Gate: "gate2+gate4",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				// Make a real content change, commit it, then commit again
				// over an unchanged tree: the second commit is the replay the
				// gate names and the first is this row's CALIBRATION.
				//
				// The calibration is not optional. "The unchanged-content
				// commit did not build" is evidence only where a commit that
				// really did change content is known to build, and since the
				// consumer gate (internal/indexer/dedicated_base_startup.go:
				// 774-779) a family with no dependent checkout builds and
				// publishes for NEITHER — which is how this row came to fail a
				// daemon that was doing exactly what the branch promises.
				// Measuring the landing commit turns each clause into either a
				// real assertion or a named "not asserted", and never into a
				// pass that means nothing (e2eMatrixUnchangedCommitVerdict).
				//
				// The content change is made HERE rather than inherited from
				// whatever the previous case left dirty: h.commit stages with
				// --allow-empty, so borrowing the content would make every
				// "a commit that really did change content …" sentence depend
				// on case ordering, which GX_E2E_MATRIX_CASES can break. The
				// case advances its own file's revision, and the landing
				// commit's tree is then compared with its parent's, so the
				// calibration is credited with content only when git agrees
				// that content moved.
				rel, index := h.rel(9), h.index(9)
				next := h.revision[index] + 1
				h.rewrite(rel, func(source string) string { return h.edited(source, index, next) })
				h.waitPresent(sustainedIOProbeName(index, next), rel)
				h.revision[index] = next
				h.settle()

				h.rebaseline()
				calibrationBefore := h.baseline
				parentTree, parentErr := h.headTree()
				h.commit("land a real content change")
				h.settle()
				landedTree, landedErr := h.headTree()
				switch {
				case parentErr != nil || landedErr != nil:
					err := parentErr
					if err == nil {
						err = landedErr
					}
					h.note("gate-4 calibration: the landing commit's tree could not be read, so it is NOT credited with content and every clause resting on it is reported as not asserted: " + err.Error())
				case parentTree == landedTree:
					h.note("gate-4 calibration: the landing commit changed no content (tree " + landedTree + " is its parent's), so every clause resting on it is reported as not asserted")
				default:
					h.commitCalibrationContent = true
				}
				calibrationAfter := h.catalog()
				if delta, err := h.deltaSince(); err != nil {
					h.commitCalibrationErr = err.Error()
				} else {
					h.commitCalibration = delta
					h.commitCalibrationSeq = calibrationAfter.Sequence - calibrationBefore.Sequence
					h.note(fmt.Sprintf("gate-4 calibration — a commit that %s moved observed[%s] build[%s] seq %+d",
						map[bool]string{true: "really did change content", false: "was NOT measured to change content"}[h.commitCalibrationContent],
						strings.Join(e2eMatrixMovedSeries(delta, e2eMatrixCommitObservationSeries()), " "),
						strings.Join(e2eMatrixMovedSeries(delta, e2eMatrixAllocationSeries()), " "),
						h.commitCalibrationSeq))
				}
				// Landing a real content change legitimately publishes; the
				// measured window is the commit AFTER it.
				h.rebaseline()
				h.git("commit", "--allow-empty", "-m", "unchanged content")
				return e2eMatrixEffect{
					Files:   []string{rel},
					Present: []e2eMatrixSighting{{sustainedIOProbeName(index, h.revision[index]), rel}},
					Note:    "a commit that changes no content at all",
				}
			},
		},
		{
			Name: "manifest_only_change", Gate: "gate1+gate3",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				h.rewrite("go.mod", func(source string) string {
					return source + "\n// e2eMatrix: manifest-only change\n"
				})
				h.commit("manifest only")
				return e2eMatrixEffect{
					Files: []string{h.rel(0), "go.mod"}, Symbols: []e2eMatrixSighting{{h.leaf(0), h.rel(0)}},
					Present: []e2eMatrixSighting{{h.probe(0), h.rel(0)}},
					Note:    "the module manifest changed and no source file did",
				}
			},
		},
		{
			Name: "configuration_change", Gate: "gate1",
			Apply: func(h *e2eMatrixEditHarness) e2eMatrixEffect {
				// A repo-local .gortex.yaml exclude is a producer-policy
				// change: the excluded package must leave both indexes.
				h.write(".gortex.yaml", "exclude:\n  - \"p003/**\"\n")
				h.commit("configuration change")
				excluded := h.relFor(3)
				h.settle()
				// Same reading as untracked_path: whether a newly committed
				// exclude withdraws already-indexed content is a product
				// behaviour. Gate 1 decides whether this view and a fresh
				// isolated index of the same tree — which reads the same
				// exclude from scratch — agree about the excluded package, and
				// the oracle compares the excluded leaf on both sides. The
				// branch's answer is recorded either way.
				//
				// And they DISAGREE. Measured at the final source, on three
				// samples a minute apart: Fn00003S0 (p003/file00003.go, the
				// package the committed .gortex.yaml excludes) is still served
				// by the incrementally maintained view and absent from a fresh
				// isolated index of the same tree. A producer-policy change
				// that lands as a commit is applied by the cold path and does
				// not withdraw content the incremental path already holds. The
				// row stays RED: the two indexes of one tree disagree, which is
				// the gate, whatever the right withdrawal policy turns out to
				// be.
				h.observe("the excluded package", h.leafFor(3), excluded)
				return e2eMatrixEffect{
					Files:   []string{h.rel(0)},
					Observe: []e2eMatrixSighting{{h.leafFor(3), excluded}},
					Present: []e2eMatrixSighting{{h.probe(0), h.rel(0)}},
					Note:    "an exclude added to the repository's own configuration",
				}
			},
		},
	}
}

// ---------------------------------------------------------------- harness ---

type e2eMatrixEditHarness struct {
	t       *testing.T
	f       *issue767Fixture
	db      *sql.DB
	table   *e2eMatrixTable
	binary  string
	spec    sustainedIOFixtureSpec
	oracle  bool
	backoff time.Duration

	// rotation is the per-case file assignment, so two cases never edit the
	// same file and a later case cannot be explained by an earlier one.
	rotation []int
	revision map[int]int

	// baseline is the catalog observation the case's own measurement window
	// opens on. A case whose setup legitimately publishes before the part it
	// measures (committing what an earlier case left dirty, say) re-opens
	// the window with rebaseline so the setup is not charged to it.
	baseline     e2eMatrixCatalog
	countersBase map[string]int64
	countersErr  error

	// notes and unmetSteps are this case's non-fatal observations: notes are
	// recorded on the row, unmetSteps also fail it and switch the gate-1
	// oracle off for it. Both are cleared per case by run.
	notes      []string
	unmetSteps []string
	// unmeasurable is a claim the case asked for that this instrument cannot
	// carry — a gate-4 reuse reading whose series a real rebuild did not move.
	// It never fails the row and it never lets it claim a confirmation: the
	// row becomes a named SKIP saying which claim could not be scored.
	unmeasurable []string

	// commitCalibration is what a commit that REALLY changed content did, in
	// this fixture, on this daemon. commit_unchanged_content takes it over
	// the commit that lands the previous case's dirty edit and scores its own
	// unchanged-content commit against it, so each of its four clauses is
	// asserted only where a real commit is shown to move the same witness.
	commitCalibration    map[string]int64
	commitCalibrationSeq int64
	commitCalibrationErr string
	// commitCalibrationContent is the measured fact that the calibration
	// commit really did change content (its tree moved), rather than the
	// assumption that it did.
	commitCalibrationContent bool
}

func (h *e2eMatrixEditHarness) index(slot int) int { return h.rotation[slot%len(h.rotation)] }

func (h *e2eMatrixEditHarness) relFor(index int) string {
	return sustainedIOFilePath(index%h.spec.Packages, index)
}

func (h *e2eMatrixEditHarness) rel(slot int) string { return h.relFor(h.index(slot)) }

func (h *e2eMatrixEditHarness) leafFor(index int) string { return fmt.Sprintf("Fn%05dS0", index) }

func (h *e2eMatrixEditHarness) leaf(slot int) string { return h.leafFor(h.index(slot)) }

func (h *e2eMatrixEditHarness) probe(slot int) string {
	index := h.index(slot)
	return sustainedIOProbeName(index, h.revision[index])
}

func (h *e2eMatrixEditHarness) path(rel string) string {
	return filepath.Join(h.f.primary, filepath.FromSlash(rel))
}

func (h *e2eMatrixEditHarness) read(rel string) string {
	h.t.Helper()
	source, err := os.ReadFile(h.path(rel))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(source)
}

func (h *e2eMatrixEditHarness) write(rel, content string) {
	h.t.Helper()
	h.f.write(h.path(rel), content)
}

// rewrite applies a transformation and refuses a no-change rewrite: a case that
// silently edited nothing would pass its own oracle comparison for free.
func (h *e2eMatrixEditHarness) rewrite(rel string, edit func(string) string) {
	h.t.Helper()
	source := h.read(rel)
	edited := edit(source)
	if edited == source {
		h.t.Fatalf("case edit of %s changed nothing", rel)
	}
	h.write(rel, edited)
}

// rewriteFile is rewrite without the change check, for the undo half of a round
// trip (which is allowed to restore the original bytes exactly).
func (h *e2eMatrixEditHarness) rewriteFile(rel string, edit func(string) string) {
	h.t.Helper()
	h.write(rel, edit(h.read(rel)))
}

func (h *e2eMatrixEditHarness) edited(source string, index, revision int) string {
	h.t.Helper()
	out, err := sustainedIOEditFileSource(source, index, revision)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *e2eMatrixEditHarness) git(args ...string) {
	h.t.Helper()
	h.f.git(h.f.primary, args...)
}

// headTree is the tree object HEAD points at. It is the witness that a commit
// carried content: commit stages with --allow-empty, so a landing commit whose
// tree equals its parent's changed nothing, and a calibration taken over it
// must not be quoted as "a commit that really did change content".
func (h *e2eMatrixEditHarness) headTree() (string, error) {
	out, err := h.f.tryGit(h.f.primary, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD^{tree}: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// commit stages everything and commits; an empty tree difference is allowed so
// a case that only changed a file mode or nothing at all still produces a
// commit.
func (h *e2eMatrixEditHarness) commit(message string) {
	h.t.Helper()
	h.git("add", "-A")
	h.git("commit", "--allow-empty", "-m", "e2eMatrix: "+message)
}

// settle waits for the daemon to go quiet, without failing anything: a busy
// host that keeps a generation counter moving has not, by that fact, broken a
// gate. Non-convergence is recorded on the row instead (see the soft-wait note
// in e2e_matrix_noop_test.go).
func (h *e2eMatrixEditHarness) settle() {
	if !e2eMatrixSoftSettle(h.t.Context(), h.db, 2*time.Minute) {
		h.note("the daemon never produced three stable generation samples within 2m: this step's observation was taken over a moving target")
	}
}

// observe records what the daemon currently says about one name and one file,
// without asserting it. It is how a case states a product-behaviour question
// whose answer is not a gate (see e2eMatrixEffect.Observe): the row carries the
// branch's answer, and the gate-1 oracle still compares that name across both
// indexes.
func (h *e2eMatrixEditHarness) observe(label, name, rel string) {
	if e2eMatrixAbsentFrom(h.f, name, h.path(rel)) {
		h.note(label + ": not served — " + name + " does not answer from " + rel)
		return
	}
	h.note(label + ": " + e2eMatrixAbsenceEvidence(h.f, name, h.path(rel)))
}

// note records something a reader has to know about this row without failing it.
func (h *e2eMatrixEditHarness) note(text string) {
	h.notes = append(h.notes, text)
}

// unmet records a step the daemon never carried out. The case keeps running —
// later steps still exercise the daemon and are still worth recording — but its
// row is failed with this reason and the gate-1 oracle is skipped: comparing a
// view that never reached the state the case asked for would report a
// difference the harness invented rather than one the branch produced.
func (h *e2eMatrixEditHarness) unmet(format string, args ...any) {
	h.unmetSteps = append(h.unmetSteps, fmt.Sprintf(format, args...))
}

// waitPresent and waitAbsent are the fixture's symbol waits made non-fatal, so
// an unmet wait fails one row instead of ending the matrix.
func (h *e2eMatrixEditHarness) waitPresent(name, rel string) {
	h.t.Helper()
	ok := e2eMatrixSoftAwait(h.t.Context(), e2eMatrixProbeTimeout, 500*time.Millisecond, func() bool {
		found, err := h.f.trySearchSymbolIn(h.f.primary, name, h.path(rel))
		return err == nil && found
	})
	if !ok {
		h.unmet("%s never answered from %s within %s", name, rel, e2eMatrixProbeTimeout)
	}
}

func (h *e2eMatrixEditHarness) waitAbsent(name, rel string) {
	h.t.Helper()
	ok := e2eMatrixSoftAwait(h.t.Context(), e2eMatrixAbsenceTimeout, 500*time.Millisecond, func() bool {
		return e2eMatrixAbsentFrom(h.f, name, h.path(rel))
	})
	if !ok {
		h.unmet("%s was never withdrawn from %s within %s: %s", name, rel, e2eMatrixAbsenceTimeout,
			e2eMatrixAbsenceEvidence(h.f, name, h.path(rel)))
	}
}

func (h *e2eMatrixEditHarness) counters() (map[string]int64, error) {
	output, err := h.f.tryCommand(e2eMatrixCommandTimeout, h.f.primary, "daemon", "status", "--format", "json", "--no-progress")
	if err != nil {
		return nil, fmt.Errorf("daemon status --format json unavailable: %w", err)
	}
	return sustainedIOParseStatusCounters(output)
}

func (h *e2eMatrixEditHarness) resetCounters() { h.countersBase, h.countersErr = h.counters() }

// rebaseline re-opens the measurement window: both the catalog observation and
// the counter snapshot the closing half subtracts from. A case calls it when
// its own setup had to publish something before the behaviour it measures.
func (h *e2eMatrixEditHarness) rebaseline() {
	h.baseline = h.catalog()
	h.resetCounters()
}

// swapT installs a case's own *testing.T on the harness AND on the shared
// fixture, and returns the restore.
//
// The fixture half matters: the fixture's own operations (git, the CLI, file
// writes) are fatal on whatever *testing.T it holds, and it holds the matrix's
// parent test. Without the swap, a git command that fails inside one case ends
// the whole matrix; with it, the failure lands on that case's subtest and the
// remaining cases are still measured and reported. The restore is not optional:
// the parent's cleanup calls the fixture back after the subtests are over, and
// a stale subtest T would panic there.
func (h *e2eMatrixEditHarness) swapT(t *testing.T) func() {
	previous, previousFixture := h.t, h.f.t
	h.t, h.f.t = t, t
	return func() { h.t, h.f.t = previous, previousFixture }
}

// e2eMatrixDirtyBuildSeries is the series that says a working-tree layer was built
// from scratch. Re-adopting a retained layer has no outcome label of its own in
// the shipped vocabulary, so reuse is read as this series NOT rising
// (internal/indexer/checkout_coordinator.go:1087-1092, which says so in those
// words).
//
// "Not rising" is only evidence when the series is known to rise for the thing
// it is supposed to detect, and the adversarial review found it is not: no
// dirty build in either matrix ever moved a coordinator_cycle series, including
// the matrix-1 control where a dirty rebuild provably happened. The emitter
// exists (checkout_coordinator.go:1094-1098, `if out.DirtyBuilt`) but this
// fixture's dirty builds never reach it. So a gate-4 reuse claim is no longer
// read straight off this series: e2eMatrixReuseVerdict requires a CALIBRATION — the
// same series over a step that really did rebuild — and, when the calibration
// shows the series stayed still there too, reports the claim as NOT MEASURABLE
// instead of as reuse.
func e2eMatrixDirtyBuildSeries() []string {
	return []string{e2eMatrixSeries(viewmetrics.CoordinatorCycleTotal, "outcome="+viewmetrics.OutcomeBuiltDirty)}
}

// e2eMatrixCommitObservationSeries is the family that moves when the daemon
// PROCESSES a HEAD movement, whatever it then decides to do about it.
//
// It is the clause that survives the consumer gate. Since
// internal/indexer/dedicated_base_startup.go:774-779 declines a committed-base
// publication for a family with no dependent checkout, a single-checkout
// fixture builds and publishes nothing for ANY commit — so "the build series
// did not move" carries no information there, and the original gate-4
// assertion ("some claim/replay counter must move") failed an unchanged-content
// commit for doing exactly what the branch now promises. What the daemon must
// still do is SEE the commit and settle it: the advance observer records the
// dispatch, and the publication records its outcome — including the declined
// one, whose whole point is that it performed zero catalog DML
// (publicationOutcome, dedicated_base_startup.go:657-671).
//
// A commit the daemon ignored entirely moves none of these, which is what
// makes the clause falsifiable on a fixture where nothing is ever built.
func e2eMatrixCommitObservationSeries() []string {
	return []string{
		e2eMatrixSeries(viewmetrics.DedicatedBaseAdvanceTotal, "outcome="+viewmetrics.AdvanceDispatched),
		e2eMatrixSeries(viewmetrics.DedicatedBaseAdvanceTotal, "outcome="+viewmetrics.AdvanceRepeat),
		e2eMatrixSeries(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationSkipped),
		e2eMatrixSeries(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationReadopted),
		e2eMatrixSeries(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationCoalesced),
		e2eMatrixSeries(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationPublished),
		e2eMatrixSeries(viewmetrics.CoordinatorCycleTotal, "outcome="+viewmetrics.OutcomeAdoptedCommit),
	}
}

// e2eMatrixUnchangedCommitInput is everything the unchanged-content commit row is
// scored from: one calibration window taken over a commit that really did
// change content, and one measured window taken over the commit that did not.
type e2eMatrixUnchangedCommitInput struct {
	// CalibrationErr is non-empty when the calibration window's counters
	// could not be read at all.
	CalibrationErr string
	// CalibrationContent records that the calibration commit REALLY changed
	// content: its tree differs from its parent's. It is not assumed, because
	// the harness commits with --allow-empty and the case would otherwise
	// borrow its content from whatever the previous case happened to leave
	// dirty — an undeclared coupling a case filter can break. A calibration
	// commit that carried no content proves nothing about what a real content
	// commit does, so every clause that rests on it says so by name instead of
	// quoting an authority it does not have.
	CalibrationContent bool
	Calibration        map[string]int64
	CalibrationSeq     int64
	Window             map[string]int64
	WindowSeq          int64
}

// e2eMatrixUnchangedCommitVerdict scores gate 2 + gate 4 for a commit that changed
// no content, against what the same fixture did for a commit that changed some.
//
// Every clause is gated on its own calibration, because the three witnesses
// answer to different lanes and the consumer gate kills two of them on a
// single-checkout family:
//
//   - OBSERVATION (e2eMatrixCommitObservationSeries): the daemon saw the commit and
//     settled it. Live wherever HEAD movement is observed at all, so this is
//     the clause that keeps the row from being vacuous.
//   - BUILD (e2eMatrixAllocationSeries): nothing was built or published.
//   - REPLAY (e2eMatrixReplaySeries): where commits do build, an unchanged-content
//     commit must show the claim/replay family instead — the original gate-4
//     assertion, now gated on the calibration that makes it mean something.
//   - ALLOCATION (the sequence): no generation was allocated.
//
// The gating is asymmetric, and the asymmetry is the whole point:
//
//   - OBSERVATION is a POSITIVE claim — the daemon must have reacted — so it is
//     sound only where the same witness is shown to move for a commit that
//     really did change content. Uncalibrated, it is unmeasurable.
//   - BUILD and ALLOCATION are NEGATIVE claims — an unchanged-content commit
//     must build, publish and allocate NOTHING. A daemon that does one of those
//     things has violated gate 2 whether or not this fixture is calibrated, so
//     the violation is scored FIRST and unconditionally. Only the ABSENCE of a
//     violation needs a calibration to mean anything, and where the calibration
//     cannot supply it the absence is recorded as NOT ASSERTED rather than
//     counted as a pass. Ordering these the other way round — gating first —
//     deletes the assertion: on a fixture whose calibration never allocates,
//     every allocating daemon would score PASS.
//
// It is pure so that every clause and every gating is pinned without a daemon.
func e2eMatrixUnchangedCommitVerdict(in e2eMatrixUnchangedCommitInput) (failures, recorded, unmeasurable []string) {
	if in.CalibrationErr != "" {
		return nil, nil, []string{"gate2+gate4: the unchanged-content commit could not be scored — the calibration window's counters were unreadable (the daemon-status views counter block emits them): " + in.CalibrationErr}
	}
	join := func(values []string) string { return strings.Join(values, " ") }

	// void names why the calibration window cannot lend its authority to a
	// clause. A commit that changed no content is not a calibration at all.
	void := ""
	if !in.CalibrationContent {
		void = "the calibration commit carried no content — its tree is identical to its parent's, so nothing in this fixture is known to have really changed"
	}

	calObserved := e2eMatrixMovedSeries(in.Calibration, e2eMatrixCommitObservationSeries())
	windowObserved := e2eMatrixMovedSeries(in.Window, e2eMatrixCommitObservationSeries())
	switch {
	case void != "":
		unmeasurable = append(unmeasurable, "gate4: "+void+
			", so this row cannot tell an unchanged-content commit the daemon settled from one it never saw")
	case len(calObserved) == 0:
		unmeasurable = append(unmeasurable, "gate4: a commit that really did change content moved none of ["+
			join(e2eMatrixCommitObservationSeries())+"] in this fixture, so this row cannot tell an unchanged-content commit the daemon settled from one it never saw")
	case len(windowObserved) == 0:
		failures = append(failures, "gate4: the daemon reported nothing at all for an unchanged-content commit; a real content commit moved ["+
			join(calObserved)+"] in the same fixture")
	default:
		recorded = append(recorded, "gate4: the unchanged-content commit was observed and settled as ["+join(windowObserved)+"]")
	}

	calBuilt := e2eMatrixMovedSeries(in.Calibration, e2eMatrixAllocationSeries())
	windowBuilt := e2eMatrixMovedSeries(in.Window, e2eMatrixAllocationSeries())
	switch {
	// The violation first, and unconditionally: building or publishing for a
	// commit that changed nothing is a gate-2 failure on any fixture.
	case len(windowBuilt) > 0:
		detail := "gate2: an unchanged-content commit built or published [" + join(windowBuilt) + "]"
		if void == "" && len(calBuilt) > 0 {
			detail += ", where a real content commit built [" + join(calBuilt) + "]"
		}
		failures = append(failures, detail)
	case void != "":
		recorded = append(recorded, "gate2 build clause NOT ASSERTED: "+void+
			", so the same series staying still over an unchanged-content commit carries no information")
	case len(calBuilt) == 0:
		recorded = append(recorded, "gate2 build clause NOT ASSERTED: a commit that really did change content built and published nothing here either — "+
			"a family with no dependent checkout defers its committed-base publication (internal/indexer/dedicated_base_startup.go:776) — "+
			"so the same series staying still over an unchanged-content commit carries no information")
	default:
		if replayed := e2eMatrixMovedSeries(in.Window, e2eMatrixReplaySeries()); len(replayed) == 0 {
			failures = append(failures, "gate4: an unchanged-content commit reported no reuse, while a real content commit in the same fixture built ["+
				join(calBuilt)+"]")
		} else {
			recorded = append(recorded, "gate4: reuse ["+join(replayed)+"] where a real content commit built ["+join(calBuilt)+"]")
		}
	}

	switch {
	// Same ordering, same reason: an allocation over an unchanged tree is a
	// gate-2 violation whether or not this fixture ever allocates.
	case in.WindowSeq != 0:
		detail := fmt.Sprintf("gate2: an unchanged-content commit allocated a generation (seq %+d)", in.WindowSeq)
		if void == "" && in.CalibrationSeq != 0 {
			detail += fmt.Sprintf(" where a real content commit moved it %+d", in.CalibrationSeq)
		} else {
			detail += " — allocating for a commit that changed no content is a gate-2 violation on any fixture, calibrated or not"
		}
		failures = append(failures, detail)
	case void != "":
		recorded = append(recorded, "gate2 allocation clause NOT ASSERTED: "+void+
			", so an unmoved sequence over an unchanged-content commit carries no information")
	case in.CalibrationSeq == 0:
		recorded = append(recorded, fmt.Sprintf("gate2 allocation clause NOT ASSERTED: a commit that really did change content allocated no generation here either (seq %+d), so an unmoved sequence over an unchanged-content commit carries no information", in.CalibrationSeq))
	default:
		recorded = append(recorded, fmt.Sprintf("gate2: no generation allocated, where a real content commit moved the sequence %+d", in.CalibrationSeq))
	}
	return failures, recorded, unmeasurable
}

// scoreUnchangedCommit folds the unchanged-content verdict into the row and
// returns the failures for the caller to report.
//
// Every routing lives here so that all three are pinned by a unit test: a
// failure marks the row FAILED and rides on its detail, a scored clause rides
// on the detail, and a clause this fixture cannot score goes to h.unmeasurable
// — which is what downgrades a would-be PASS to a named SKIP in file(). The
// caller only reports the returned failures; the row is already FAILED, and
// render() reports a FAILED row from the table regardless.
func (h *e2eMatrixEditHarness) scoreUnchangedCommit(row *e2eMatrixRow, in e2eMatrixUnchangedCommitInput, counters map[string]int64) []string {
	failures, scored, cannot := e2eMatrixUnchangedCommitVerdict(in)
	for _, failure := range failures {
		e2eMatrixMarkFailed(row, failure+"; counters: "+e2eMatrixTruncate(e2eMatrixCanonical(counters), 300))
	}
	for _, note := range scored {
		row.Detail = strings.TrimSpace(row.Detail + " | " + note)
	}
	h.unmeasurable = append(h.unmeasurable, cannot...)
	return failures
}

// e2eMatrixReuseVerdict decides what "the build series did not move" means, given
// what a step that really did rebuild did to the SAME series.
//
// It is pure so the decision is pinned without a daemon: calibration silent →
// not measurable; calibration moved and the window moved → rebuilt; calibration
// moved and the window did not → reused.
func e2eMatrixReuseVerdict(label string, calibration, window map[string]int64, series []string) (text string, measurable, reused bool) {
	movedInCalibration := e2eMatrixMovedSeries(calibration, series)
	movedInWindow := e2eMatrixMovedSeries(window, series)
	if len(movedInCalibration) == 0 {
		return label + ": NOT MEASURABLE — a step that really did rebuild moved none of [" +
			strings.Join(series, " ") + "] in this fixture, so the same series staying still here carries no information", false, false
	}
	if len(movedInWindow) > 0 {
		return label + ": REBUILT " + strings.Join(movedInWindow, " "), true, false
	}
	return label + ": reused (" + strings.Join(series, " ") + " moved " +
		strings.Join(movedInCalibration, " ") + " on a real rebuild and did not move here)", true, true
}

// deltaSince closes a measurement sub-window a case opened with rebaseline and
// returns the counter delta over it. An unreadable counter set is reported as
// an error rather than as a fabricated zero.
func (h *e2eMatrixEditHarness) deltaSince() (map[string]int64, error) {
	h.t.Helper()
	after, err := h.counters()
	if h.countersErr != nil {
		return nil, h.countersErr
	}
	if err != nil {
		return nil, err
	}
	return sustainedIOCounterDelta(h.countersBase, after), nil
}

// reuseSince closes a sub-window and scores a gate-4 reuse claim against a
// calibration window taken over a step that really did rebuild. It is how a
// case scopes a gate-4 claim to one step of a multi-step body — the undo half
// of an edit/undo/redo round trip is a reuse claim, the edit before it is not.
//
// Unreadable counters are a named skip sentence pointing at the block that
// emits them, never a silent pass and never a fabricated zero. A claim the
// instrument cannot carry is recorded as unmeasurable, which turns the row into
// a named SKIP rather than leaving a "confirmed" that means nothing.
func (h *e2eMatrixEditHarness) reuseSince(label string, calibration map[string]int64, forbidden []string) string {
	h.t.Helper()
	delta, err := h.deltaSince()
	if err != nil {
		reason := label + ": not scored — daemon status views counters unavailable (the daemon-status views counter block emits them): " + err.Error()
		h.unmeasurable = append(h.unmeasurable, reason)
		return reason
	}
	text, measurable, reused := e2eMatrixReuseVerdict(label, calibration, delta, forbidden)
	switch {
	case !measurable:
		h.unmeasurable = append(h.unmeasurable, text)
	case !reused:
		h.t.Errorf("gate4: %s rebuilt instead of reusing the retained layer: %s", label, text)
	}
	return text
}

func (h *e2eMatrixEditHarness) catalog() e2eMatrixCatalog {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return e2eMatrixReadCatalog(ctx, h.db)
}

// ----------------------------------------------------------- the oracle ---

// e2eMatrixRootToken is the placeholder a probe payload carries where an absolute
// path belongs. Each daemon is asked with the token replaced by ITS OWN root:
// the two indexes live under different roots, and a payload built from one
// daemon's root is answered by the other with "file_not_indexed" — a difference
// the harness would have invented rather than observed.
const e2eMatrixRootToken = "<ROOT>"

// e2eMatrixProbesFor turns a case's effect into the projection set. Every probe
// names the gate-1 clause it evidences.
func e2eMatrixProbesFor(effect e2eMatrixEffect) []e2eMatrixProbe {
	var probes []e2eMatrixProbe
	for _, rel := range effect.Files {
		probes = append(probes, e2eMatrixProbe{
			Key: "summary:" + rel, Clause: "locations+metadata+edges+ownership+visibility",
			Facade:  "read",
			Payload: map[string]any{"operation": "summary", "path": e2eMatrixRootToken + string(filepath.Separator) + filepath.FromSlash(rel)},
		})
	}
	for _, symbol := range effect.Symbols {
		id := e2eMatrixSymbolID(symbol.Rel, symbol.Name)
		probes = append(probes,
			e2eMatrixProbe{Key: "usages:" + id, Clause: "incoming edges",
				Facade:  "relations",
				Payload: map[string]any{"operation": "usages", "target": map[string]any{"symbol": id}}},
			e2eMatrixProbe{Key: "source:" + id, Clause: "source bytes + span",
				Facade:  "read",
				Payload: map[string]any{"operation": "source", "target": map[string]any{"symbol": id}}},
		)
	}
	named := append([]e2eMatrixSighting{}, effect.Present...)
	named = append(named, effect.Absent...)
	named = append(named, effect.Observe...)
	for _, sighting := range named {
		probes = append(probes, e2eMatrixProbe{
			Key: "symbols:" + sighting.Name, Clause: "resolved target + deletion absence",
			Facade: "search", Name: sighting.Name,
			Payload: map[string]any{"operation": "symbols", "query": sighting.Name,
				"options": map[string]any{"limit": 25, "query_class": "symbol", "expand": "off"}},
		})
	}
	for _, text := range effect.Texts {
		probes = append(probes, e2eMatrixProbe{
			Key: "text:" + text, Clause: "text lane",
			Facade:  "search",
			Payload: map[string]any{"operation": "text", "query": text},
		})
	}
	return e2eMatrixDedupeProbes(probes)
}

func e2eMatrixDedupeProbes(probes []e2eMatrixProbe) []e2eMatrixProbe {
	seen := map[string]bool{}
	out := make([]e2eMatrixProbe, 0, len(probes))
	for _, probe := range probes {
		if seen[probe.Key] {
			continue
		}
		seen[probe.Key] = true
		out = append(out, probe)
	}
	return out
}

// e2eMatrixSymbolID is the graph identity of a declaration, as the product spells
// it: "<repo prefix>/<repo-relative path>::<name>".
func e2eMatrixSymbolID(rel, name string) string {
	return issue767FixturePrefix + "/" + rel + "::" + name
}

// e2eMatrixProject asks one daemon every probe and returns the canonical rendering
// of each answer. A transport failure is recorded as the answer rather than
// failing the test: two daemons that both refuse a probe agree, and a daemon
// that refuses one the other answered is exactly the difference to report.
func e2eMatrixProject(f *issue767Fixture, probes []e2eMatrixProbe, drop map[string]bool) map[string]string {
	out := make(map[string]string, len(probes))
	for _, probe := range probes {
		payload, err := json.Marshal(probe.Payload)
		if err != nil {
			out[probe.Key] = "marshal error: " + err.Error()
			continue
		}
		// The payload is written against <ROOT>; every daemon is asked about
		// its own checkout.
		request := strings.ReplaceAll(string(payload), e2eMatrixRootToken, f.primary)
		output, err := f.tryCommand(e2eMatrixCommandTimeout, f.primary, "call", probe.Facade,
			"--index", f.primary, "--json", request, "--format", "json")
		if err != nil {
			out[probe.Key] = "call error: " + err.Error() + ": " + sustainedIOTail(output)
			continue
		}
		var value any
		if err := json.Unmarshal(output, &value); err != nil {
			out[probe.Key] = "decode error: " + err.Error() + ": " + sustainedIOTail(output)
			continue
		}
		normalized := e2eMatrixNormalize(value, f.primary, drop)
		if probe.Name != "" {
			normalized = e2eMatrixReduceToName(normalized, probe.Name)
		}
		out[probe.Key] = e2eMatrixCanonical(normalized)
	}
	return out
}

// e2eMatrixCopyTree copies a checkout's working tree — the exact bytes the
// incremental daemon is serving — into a fresh fixture's root. The Git
// directory is deliberately not copied: the oracle commits the same content
// under its own history, and gate 1 compares the index of a tree, not the
// commit that produced it.
func e2eMatrixCopyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return os.MkdirAll(filepath.Join(to, rel), 0o700)
		}
		destination := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(destination)
			return os.Symlink(target, destination)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, source, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// e2eMatrixFreshIndex starts a throwaway isolated daemon over a copy of the tree and
// returns it ready. The caller stops it as soon as the comparison is done: one
// extra daemon at a time, never seventeen.
func e2eMatrixFreshIndex(t *testing.T, binary, tree string) *issue767Fixture {
	t.Helper()
	fresh := newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		e2eMatrixCopyTree(t, tree, f.primary)
	})
	fresh.start()
	fresh.awaitSymbolIn(fresh.primary, sustainedIOPrimaryMarker, filepath.Join(fresh.primary, "marker.go"), e2eMatrixProbeTimeout)
	// A fresh index that has not gone quiet is still sampled: the oracle
	// re-samples on difference, so a converging index costs a retry rather
	// than a wrong verdict, while a hard failure here would cost the case.
	if !e2eMatrixSoftSettle(t.Context(), fresh.openReadOnly(), 2*time.Minute) {
		t.Logf("the fresh isolated index did not produce three stable generation samples within 2m; sampling it anyway")
	}
	return fresh
}

// run executes one case, files its row and — unless the oracle is switched off
// — compares the incremental view against a fresh isolated index of the tree
// the case produced.
func (h *e2eMatrixEditHarness) run(c e2eMatrixEditCase) {
	h.t.Helper()
	started := time.Now()
	h.notes, h.unmetSteps, h.unmeasurable = nil, nil, nil
	h.commitCalibration, h.commitCalibrationSeq, h.commitCalibrationErr = nil, 0, ""
	h.commitCalibrationContent = false
	h.rebaseline()
	row := e2eMatrixRow{Case: c.Name, Gate: c.Gate, Status: e2eMatrixStatusPass}

	effect := c.Apply(h)
	for _, sighting := range effect.Present {
		h.waitPresent(sighting.Name, sighting.Rel)
	}
	for _, sighting := range effect.Absent {
		h.waitAbsent(sighting.Name, sighting.Rel)
	}
	h.settle()

	before, after := h.baseline, h.catalog()
	row.Seconds = time.Since(started).Seconds()
	row.Bookkeeping = e2eMatrixBookkeepingDelta(before, after)
	row.Detail = effect.Note

	// Gate 4: what did this case cost — reuse or rebuild? Recorded for every
	// case from the daemon's own counters; the replay case asserts on it.
	// A case that asserts on a series it could not read is a named skip
	// pointing at the counter block that emits it, never a silent pass.
	assertsReuse := c.Name == "commit_unchanged_content"
	afterCounters, err := h.counters()
	switch {
	case h.countersErr != nil || err != nil:
		reason := "daemon status views counters unavailable (the daemon-status views counter block emits them)"
		row.Detail = strings.TrimSpace(row.Detail + " | " + reason)
		if assertsReuse {
			row.Status = e2eMatrixStatusSkip
			row.Detail = reason + "; the gate-4 reuse assertion could not run"
			h.file(row)
			return
		}
	default:
		delta := sustainedIOCounterDelta(h.countersBase, afterCounters)
		row.Counters = delta
		built := e2eMatrixMovedSeries(delta, e2eMatrixAllocationSeries())
		replayed := e2eMatrixMovedSeries(delta, e2eMatrixReplaySeries())
		row.Detail = strings.TrimSpace(row.Detail + fmt.Sprintf(" | rebuild[%s] reuse[%s] seq %+d",
			strings.Join(built, " "), strings.Join(replayed, " "), after.Sequence-before.Sequence))
		if assertsReuse {
			failures := h.scoreUnchangedCommit(&row, e2eMatrixUnchangedCommitInput{
				CalibrationErr:     h.commitCalibrationErr,
				CalibrationContent: h.commitCalibrationContent,
				Calibration:        h.commitCalibration,
				CalibrationSeq:     h.commitCalibrationSeq,
				Window:             delta,
				WindowSeq:          after.Sequence - before.Sequence,
			}, delta)
			for _, failure := range failures {
				h.t.Errorf("matrix2 %s (%s): %s", row.Case, row.Gate, failure)
			}
		}
	}

	for _, note := range h.notes {
		row.Detail = strings.TrimSpace(row.Detail + " | " + note)
	}
	if len(h.unmetSteps) > 0 {
		// The case never reached the state it asked for. That is a reported
		// failure of this row — and the gate-1 oracle is deliberately not run
		// on it, because a view that never carried the edit would differ from
		// a fresh index of the tree for a reason the harness caused.
		h.fail(&row, "the daemon never carried this case to the state it asked for, so the gate-1 oracle was not run: "+strings.Join(h.unmetSteps, " ;; "))
		h.file(row)
		return
	}

	if !h.oracle {
		row.Status = e2eMatrixStatusSkip
		row.Detail = "fresh-index oracle disabled by GX_E2E_MATRIX_ORACLE=0; " + row.Detail
		h.file(row)
		return
	}

	probes := e2eMatrixProbesFor(effect)
	drop := e2eMatrixMergedKeys(e2eMatrixVolatileKeys(), e2eMatrixProvenanceKeys())
	result := h.compareAgainstFreshIndex(probes, drop)
	differences, residual, provenance := result.Differences, result.Residual, result.Provenance
	row.Differences = differences
	verdict := e2eMatrixGateOneVerdict(e2eMatrixOracleInput{
		Probes: len(probes), Substantive: result.Substantive, Empty: result.Empty,
		Differences: differences, Residual: residual,
		Attempts: e2eMatrixOracleAttempts, Backoff: h.backoff,
	})
	switch verdict.Status {
	case e2eMatrixStatusPass:
		row.Detail = strings.TrimSpace(row.Detail + " | " + strings.Join(verdict.Recorded, " | "))
	case e2eMatrixStatusSkip:
		// Every difference is the measured semantic-metadata gap. The row is
		// a named skip pointing at the ledger row that owns it, and the full
		// difference still rides on the row.
		row.Status = e2eMatrixStatusSkip
		row.Detail = strings.TrimSpace(row.Detail + " | " + strings.Join(verdict.Recorded, " | "))
		h.t.Logf("matrix2 %s (%s): SKIP — %s", row.Case, row.Gate, row.Detail)
	default:
		h.fail(&row, strings.Join(verdict.Failures, " ;; "))
	}
	if provenance > 0 {
		row.Detail += fmt.Sprintf(" | provenance differs on %d probe(s) (asynchronous LSP enrichment; recorded, not asserted)", provenance)
	}
	h.file(row)
}

// file records one row, downgrading a would-be PASS whose case asked for a
// claim this instrument cannot carry. A gate-4 reuse reading whose series a
// real rebuild did not move is not a confirmation, so the row says which claim
// could not be scored instead of quietly keeping the PASS.
func (h *e2eMatrixEditHarness) file(row e2eMatrixRow) {
	if len(h.unmeasurable) > 0 {
		if row.Status == e2eMatrixStatusPass {
			row.Status = e2eMatrixStatusSkip
		}
		row.Detail = strings.TrimSpace(row.Detail + " | NOT MEASURABLE: " + strings.Join(h.unmeasurable, " ;; "))
	}
	h.table.add(row)
}

// compareAgainstFreshIndex builds a fresh isolated index of the current tree
// and compares it with the incremental view, retrying while the two are still
// converging. It returns the surviving structural differences and how many
// probes differ once provenance is included.
func (h *e2eMatrixEditHarness) compareAgainstFreshIndex(probes []e2eMatrixProbe, drop map[string]bool) e2eMatrixOracleResult {
	var residual []e2eMatrixDifference
	var substantive int
	var empty []string
	structural := drop
	volatileOnly := e2eMatrixVolatileKeys()
	// residualKeys additionally hides the measured semantic-metadata gap, so
	// the harness can say whether a difference is ONLY that gap or something
	// else as well.
	residualKeys := e2eMatrixMergedKeys(structural, e2eMatrixSemanticMetadataKeys())

	var differences []e2eMatrixDifference
	provenance := 0
	for attempt := 1; attempt <= e2eMatrixOracleAttempts; attempt++ {
		fresh := e2eMatrixFreshIndex(h.t, h.binary, h.f.primary)
		incrementalStructural := e2eMatrixProject(h.f, probes, structural)
		freshStructural := e2eMatrixProject(fresh, probes, structural)
		differences = e2eMatrixCompare(incrementalStructural, freshStructural, probes)
		substantive, empty = e2eMatrixSubstantiveProbes(incrementalStructural, freshStructural, probes)
		provenance = len(e2eMatrixCompare(e2eMatrixProject(h.f, probes, volatileOnly), e2eMatrixProject(fresh, probes, volatileOnly), probes))
		if len(differences) > 0 {
			residual = e2eMatrixCompare(e2eMatrixProject(h.f, probes, residualKeys), e2eMatrixProject(fresh, probes, residualKeys), probes)
		} else {
			residual = nil
		}
		fresh.stop()
		if len(differences) == 0 {
			return e2eMatrixOracleResult{Provenance: provenance, Substantive: substantive, Empty: empty}
		}
		if len(residual) == 0 {
			// The only difference is the characterized semantic-metadata
			// gap, which is sustained rather than converging: sampling
			// again would cost two more fresh indexes to learn nothing.
			return e2eMatrixOracleResult{Differences: differences, Provenance: provenance, Substantive: substantive, Empty: empty}
		}
		if attempt < e2eMatrixOracleAttempts {
			h.t.Logf("gate1 oracle attempt %d: %d probe(s) still differ after %s of convergence; sampling again",
				attempt, len(differences), h.backoff)
			select {
			case <-h.t.Context().Done():
				h.t.Fatal(h.t.Context().Err())
			case <-time.After(h.backoff):
			}
			h.settle()
		}
	}
	return e2eMatrixOracleResult{Differences: differences, Residual: residual, Provenance: provenance,
		Substantive: substantive, Empty: empty}
}

// e2eMatrixMarkFailed is the row half of fail: it marks the row FAILED and records
// why. It is pure and separate so a scoring path that produces failures can be
// exercised by a unit test without reporting them to the test that runs it;
// render() reports every FAILED row from the table, so marking is what decides
// the matrix, and fail's Errorf is the immediate, in-place report.
func e2eMatrixMarkFailed(row *e2eMatrixRow, detail string) {
	row.Status = e2eMatrixStatusFail
	row.Detail = strings.TrimSpace(row.Detail + " | " + detail)
}

func (h *e2eMatrixEditHarness) fail(row *e2eMatrixRow, detail string) {
	e2eMatrixMarkFailed(row, detail)
	h.t.Errorf("matrix2 %s (%s): %s", row.Case, row.Gate, detail)
}

// ------------------------------------------------------------ opt-in entry ---

// TestE2EMatrixEditTaxonomy is matrix 2.
func TestE2EMatrixEditTaxonomy(t *testing.T) {
	binary := e2eMatrixBinary(t)
	spec := e2eMatrixFixtureSpec(t)
	filter := e2eMatrixCaseFilter(t)

	table := e2eMatrixNewTable(t, "e2e_matrix_edits")
	defer table.render()

	f := e2eMatrixNewFixture(t, binary, spec)
	defer f.stop()

	// Every case edits its own file: a difference a later case reports must
	// not be explainable by an earlier case's edit to the same file.
	rotation := sustainedIORotationTargets(spec, len(e2eMatrixEditCaseNames()))
	if len(rotation) < len(e2eMatrixEditCaseNames()) {
		t.Fatalf("the corpus has %d rotation targets for %d cases; raise GX_E2E_MATRIX_FILES to at least %d",
			len(rotation), len(e2eMatrixEditCaseNames()), len(e2eMatrixEditCaseNames()))
	}
	h := &e2eMatrixEditHarness{
		t: t, f: f, db: f.openReadOnly(), table: table, binary: binary, spec: spec,
		oracle:   os.Getenv("GX_E2E_MATRIX_ORACLE") != "0",
		backoff:  e2eMatrixOracleBackoff(t),
		rotation: rotation,
		revision: map[int]int{},
	}
	for _, c := range e2eMatrixEditCases() {
		if filter != nil && !filter.MatchString(c.Name) {
			table.add(e2eMatrixRow{Case: c.Name, Gate: c.Gate, Status: e2eMatrixStatusSkip,
				Detail: "not selected by GX_E2E_MATRIX_CASES"})
			continue
		}
		t.Logf("matrix2 case %s (%s)", c.Name, c.Gate)
		e2eMatrixRunGuarded(t, table, c.Name, c.Gate, h.swapT, func() { h.run(c) })
	}
}

// ------------------------------------------------------------------ tests ---

func TestE2EMatrixNormalizeMakesTwoDaemonsComparable(t *testing.T) {
	left := map[string]any{
		"etag":  "aaaa",
		"nodes": []any{map[string]any{"id": "issue767/p000/a.go::B", "absolute_file_path": "/roots/left/p000/a.go"}},
		"edges": []any{
			map[string]any{"from": "x", "to": "y", "kind": "calls", "line": 8.0, "origin": "lsp_resolved", "tier": "lsp"},
			map[string]any{"from": "a", "to": "b", "kind": "reads", "line": 2.0, "origin": "ast_resolved"},
		},
	}
	right := map[string]any{
		"etag":  "bbbb",
		"nodes": []any{map[string]any{"id": "issue767/p000/a.go::B", "absolute_file_path": "/roots/right/p000/a.go"}},
		"edges": []any{
			// Same edges, opposite order, AST provenance instead of LSP.
			map[string]any{"from": "a", "to": "b", "kind": "reads", "line": 2.0, "origin": "ast_resolved"},
			map[string]any{"from": "x", "to": "y", "kind": "calls", "line": 8.0, "origin": "ast_resolved", "tier": "ast"},
		},
	}
	drop := e2eMatrixMergedKeys(e2eMatrixVolatileKeys(), e2eMatrixProvenanceKeys())
	if got, want := e2eMatrixCanonical(e2eMatrixNormalize(left, "/roots/left", drop)), e2eMatrixCanonical(e2eMatrixNormalize(right, "/roots/right", drop)); got != want {
		t.Fatalf("structural normalization did not make the two answers comparable:\n%s\n%s", got, want)
	}
	// With provenance kept, the same pair must differ: dropping it is a
	// deliberate, named exception, not an accident of the normalizer.
	volatile := e2eMatrixVolatileKeys()
	if e2eMatrixCanonical(e2eMatrixNormalize(left, "/roots/left", volatile)) == e2eMatrixCanonical(e2eMatrixNormalize(right, "/roots/right", volatile)) {
		t.Fatal("provenance difference vanished even when provenance was kept")
	}
	// A real content difference must survive normalization.
	changed := map[string]any{
		"etag":  "cccc",
		"nodes": []any{map[string]any{"id": "issue767/p000/a.go::B", "absolute_file_path": "/roots/right/p000/a.go"}},
		"edges": []any{map[string]any{"from": "a", "to": "b", "kind": "reads", "line": 2.0, "origin": "ast_resolved"}},
	}
	if e2eMatrixCanonical(e2eMatrixNormalize(left, "/roots/left", drop)) == e2eMatrixCanonical(e2eMatrixNormalize(changed, "/roots/right", drop)) {
		t.Fatal("a dropped edge was normalized away")
	}
	moved := map[string]any{
		"etag":  "dddd",
		"nodes": []any{map[string]any{"id": "issue767/p000/a.go::B", "absolute_file_path": "/roots/right/p000/a.go"}},
		"edges": []any{
			map[string]any{"from": "x", "to": "y", "kind": "calls", "line": 9.0, "origin": "ast_resolved"},
			map[string]any{"from": "a", "to": "b", "kind": "reads", "line": 2.0, "origin": "ast_resolved"},
		},
	}
	if e2eMatrixCanonical(e2eMatrixNormalize(left, "/roots/left", drop)) == e2eMatrixCanonical(e2eMatrixNormalize(moved, "/roots/right", drop)) {
		t.Fatal("a moved location was normalized away")
	}
}

func TestE2EMatrixCompareNamesTheProbeAndTheClause(t *testing.T) {
	probes := []e2eMatrixProbe{
		{Key: "summary:p000/a.go", Clause: "locations"},
		{Key: "usages:issue767/p000/a.go::B", Clause: "incoming edges"},
		{Key: "symbols:B", Clause: "resolved target + deletion absence"},
	}
	same := map[string]string{"summary:p000/a.go": "{}", "usages:issue767/p000/a.go::B": "[]", "symbols:B": "{}"}
	if differences := e2eMatrixCompare(same, same, probes); len(differences) != 0 {
		t.Fatalf("identical projections differed: %v", differences)
	}
	other := map[string]string{"summary:p000/a.go": "{}", "usages:issue767/p000/a.go::B": "[1]"}
	differences := e2eMatrixCompare(same, other, probes)
	if len(differences) != 2 {
		t.Fatalf("want a difference for the changed probe and the missing one, got %v", differences)
	}
	if differences[0].Probe != "usages:issue767/p000/a.go::B" || differences[0].Clause != "incoming edges" {
		t.Fatalf("difference did not name the probe and its gate-1 clause: %+v", differences[0])
	}
	if differences[1].Fresh != "<probe not answered>" {
		t.Fatalf("an unanswered probe was not reported as such: %+v", differences[1])
	}
}

func TestE2EMatrixProbesForCoverEveryGate1Clause(t *testing.T) {
	effect := e2eMatrixEffect{
		Files:   []string{"p000/file00000.go"},
		Symbols: []e2eMatrixSighting{{"Fn00000S0", "p000/file00000.go"}},
		Present: []e2eMatrixSighting{{"GxProbe00000Rev1", "p000/file00000.go"}},
		Absent:  []e2eMatrixSighting{{"GxProbe00000Rev0", "p000/file00000.go"}},
		Observe: []e2eMatrixSighting{{"GxEditsIgnoredSymbol", "e2eMatrix_ignored/ignored.go"}},
		Texts:   []string{"gxSalt00000"},
	}
	probes := e2eMatrixProbesFor(effect)
	byKey := map[string]e2eMatrixProbe{}
	for _, probe := range probes {
		byKey[probe.Key] = probe
	}
	for _, want := range []string{
		"summary:p000/file00000.go",
		"usages:issue767/p000/file00000.go::Fn00000S0",
		"source:issue767/p000/file00000.go::Fn00000S0",
		"symbols:GxProbe00000Rev1",
		"symbols:GxProbe00000Rev0",
		// An observed name is projected exactly like an asserted one: the
		// case does not judge it, the oracle still compares it.
		"symbols:GxEditsIgnoredSymbol",
		"text:gxSalt00000",
	} {
		probe, ok := byKey[want]
		if !ok {
			t.Fatalf("probe %q is missing; the oracle would not compare that clause", want)
		}
		if probe.Clause == "" || probe.Facade == "" || len(probe.Payload) == 0 {
			t.Fatalf("probe %q is incomplete: %+v", want, probe)
		}
	}
	// The path is written against the <ROOT> placeholder, not against one
	// daemon's root: e2eMatrixProject substitutes the answering daemon's own root,
	// and a payload hard-wired to the other daemon's root would be answered
	// "file_not_indexed" — a difference the harness invented rather than
	// observed.
	summary := byKey["summary:p000/file00000.go"]
	path, _ := summary.Payload["path"].(string)
	if want := e2eMatrixRootToken + string(filepath.Separator) + filepath.FromSlash("p000/file00000.go"); path != want {
		t.Fatalf("the summary probe path is %q, want the placeholder form %q", path, want)
	}
	for _, probe := range probes {
		rendered := e2eMatrixCanonical(probe.Payload)
		if strings.Contains(rendered, "/private/") || strings.Contains(rendered, "gx767-") {
			t.Fatalf("probe %q carries a concrete daemon root: %s", probe.Key, rendered)
		}
	}
	// Asking the same question twice would double the cost and report one
	// difference twice.
	if doubled := e2eMatrixProbesFor(effect); len(doubled) != len(probes) {
		t.Fatalf("probe set is not stable: %d vs %d", len(doubled), len(probes))
	}
	seen := map[string]bool{}
	for _, probe := range probes {
		if seen[probe.Key] {
			t.Fatalf("probe %q is duplicated", probe.Key)
		}
		seen[probe.Key] = true
	}
}

func TestE2EMatrixEditCasesCoverTheDeclaredTaxonomy(t *testing.T) {
	declared := e2eMatrixEditCaseNames()
	cases := e2eMatrixEditCases()
	if len(cases) != len(declared) {
		t.Fatalf("matrix 2 has %d cases, the declared taxonomy has %d", len(cases), len(declared))
	}
	for i, name := range declared {
		if cases[i].Name != name {
			t.Fatalf("case %d is %q, want %q (the declared taxonomy order)", i, cases[i].Name, name)
		}
		if cases[i].Apply == nil {
			t.Fatalf("case %q has no body", name)
		}
		if !strings.HasPrefix(cases[i].Gate, "gate") {
			t.Fatalf("case %q names no acceptance gate, got %q", name, cases[i].Gate)
		}
	}
	// Every class of the taxonomy has to be represented by at least one case.
	for _, class := range []string{"comment", "presentation", "directive", "body", "signature", "export",
		"import", "added", "deleted", "renamed", "mode", "symlink", "untracked", "undo", "unchanged",
		"manifest", "configuration"} {
		found := false
		for _, c := range cases {
			if strings.Contains(c.Name, class) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no case covers the %q class of the edit taxonomy", class)
		}
	}
}

func TestE2EMatrixReduceToNameKeepsTheSymbolAndDropsThePage(t *testing.T) {
	row := func(name, file string, line float64) map[string]any {
		return map[string]any{"id": "issue767/" + file + "::" + name, "name": name,
			"file_path": "issue767/" + file, "start_line": line, "visibility": "public"}
	}
	page := func(rows ...map[string]any) map[string]any {
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, r)
		}
		return map[string]any{"results": out, "total": float64(len(out)), "truncated": true}
	}
	wanted := row("GxProbe00006Rev0", "p002/file00006.go", 45)

	// Two correct indexes whose ranked pages tie differently on unrelated
	// same-shaped names must compare equal: the ranking of symbols the case
	// never touched is not a property of the tree.
	left := page(wanted, row("GxProbe00013Rev0", "p001/file00013.go", 44), row("GxProbe00021Rev0", "p001/file00021.go", 44))
	right := page(row("GxProbe00000Rev0", "p000/file00000.go", 45), wanted, row("GxProbe00004Rev0", "p000/file00004.go", 45))
	if a, b := e2eMatrixCanonical(e2eMatrixReduceToName(left, "GxProbe00006Rev0")), e2eMatrixCanonical(e2eMatrixReduceToName(right, "GxProbe00006Rev0")); a != b {
		t.Fatalf("tie-break noise survived the reduction:\n%s\n%s", a, b)
	}

	// Deletion absence: a page that no longer carries the name reduces to
	// nothing, and that is different from a page that carries it.
	gone := page(row("GxProbe00013Rev0", "p001/file00013.go", 44))
	if got := e2eMatrixCanonical(e2eMatrixReduceToName(gone, "GxProbe00006Rev0")); got != "[]" {
		t.Fatalf("a withdrawn symbol reduced to %s", got)
	}
	if e2eMatrixCanonical(e2eMatrixReduceToName(gone, "GxProbe00006Rev0")) == e2eMatrixCanonical(e2eMatrixReduceToName(left, "GxProbe00006Rev0")) {
		t.Fatal("present and absent reduced to the same answer; the probe would prove nothing")
	}

	// A real change to the symbol's own row survives: its file, its line and
	// its visibility are what the clause is about.
	for _, tc := range []struct {
		name string
		row  map[string]any
	}{
		{"moved to another file", row("GxProbe00006Rev0", "p002/other.go", 45)},
		{"moved line", row("GxProbe00006Rev0", "p002/file00006.go", 61)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := page(tc.row, row("GxProbe00013Rev0", "p001/file00013.go", 44))
			if e2eMatrixCanonical(e2eMatrixReduceToName(changed, "GxProbe00006Rev0")) == e2eMatrixCanonical(e2eMatrixReduceToName(left, "GxProbe00006Rev0")) {
				t.Fatalf("%s was reduced away", tc.name)
			}
		})
	}
	// Two rows for the same name (a duplicate the incremental view invented)
	// must not collapse into one.
	duplicated := page(wanted, wanted)
	if e2eMatrixCanonical(e2eMatrixReduceToName(duplicated, "GxProbe00006Rev0")) == e2eMatrixCanonical(e2eMatrixReduceToName(left, "GxProbe00006Rev0")) {
		t.Fatal("a duplicated row was reduced away")
	}
	// A row without an id is not a symbol row (a facet count, a suggestion).
	if got := e2eMatrixCanonical(e2eMatrixReduceToName(map[string]any{"facets": []any{map[string]any{"name": "GxProbe00006Rev0", "count": 3.0}}}, "GxProbe00006Rev0")); got != "[]" {
		t.Fatalf("a non-symbol row was taken for a symbol: %s", got)
	}
}

func TestE2EMatrixDirtyBuildSeriesIsABuildSeries(t *testing.T) {
	series := e2eMatrixDirtyBuildSeries()
	if len(series) != 1 {
		t.Fatalf("the gate-4 reuse instrument is %d series: %v", len(series), series)
	}
	declared := map[string]bool{}
	for _, name := range viewmetrics.SeriesNames() {
		declared[name] = true
	}
	if !declared[e2eMatrixSeriesName(series[0])] {
		t.Fatalf("series %q is not declared in the viewmetrics catalog; the reuse claim would read a permanent zero", series[0])
	}
	// It has to be a series that only a BUILD moves. A reuse claim phrased
	// against a series that also moves on reuse — or on merely serving a
	// request — would pass no matter what the daemon did.
	allocation := map[string]bool{}
	for _, key := range e2eMatrixAllocationSeries() {
		allocation[key] = true
	}
	if !allocation[series[0]] {
		t.Fatalf("series %q is not in the allocation family, so 'it did not move' is not evidence of reuse", series[0])
	}
	for _, key := range e2eMatrixReplaySeries() {
		if key == series[0] {
			t.Fatalf("series %q is the replay family's own series; reuse would look like a rebuild", key)
		}
	}
}

func TestE2EMatrixSymbolIDIsTheProductSpelling(t *testing.T) {
	if got, want := e2eMatrixSymbolID("p000/file00000.go", "Fn00000S0"), "issue767/p000/file00000.go::Fn00000S0"; got != want {
		t.Fatalf("symbol id %q, want %q", got, want)
	}
}

func TestE2EMatrixCopyTreeReproducesTheServedBytes(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(from, "p000"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(from, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "p000", "a.go"), []byte("package p000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "p000", "x.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, ".git", "objects", "junk"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkErr := os.Symlink(filepath.Join("p000", "a.go"), filepath.Join(from, "link.go"))
	e2eMatrixCopyTree(t, from, to)

	if source, err := os.ReadFile(filepath.Join(to, "p000", "a.go")); err != nil || string(source) != "package p000\n" {
		t.Fatalf("copied content: %q %v", source, err)
	}
	info, err := os.Stat(filepath.Join(to, "p000", "x.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if source, err := os.ReadFile(filepath.Join(to, "p000", "x.sh")); err != nil || string(source) != "#!/bin/sh\n" {
		t.Fatalf("copied executable content: %q %v", source, err)
	}
	t.Run("POSIX executable permission", func(t *testing.T) {
		portableHarnessAssertExecutableBit(t, info.Mode())
	})
	t.Run("symlink", func(t *testing.T) {
		if symlinkErr != nil {
			t.Skipf("this host refuses symlinks: %v", symlinkErr)
		}
		if _, err := os.Lstat(filepath.Join(to, "link.go")); err != nil {
			t.Fatalf("the symlink was not reproduced: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(to, "link.go")); err != nil || target != filepath.Join("p000", "a.go") {
			t.Fatalf("symlink target %q %v", target, err)
		}
	})
	if _, err := os.Stat(filepath.Join(to, ".git")); !os.IsNotExist(err) {
		t.Fatalf("the Git directory was copied into the oracle fixture: %v", err)
	}
}

func TestE2EMatrixDescribeDifferenceNamesTheEntryThatDiffers(t *testing.T) {
	left := `{"edges":[{"from":"A","kind":"calls","line":8,"to":"B"},{"from":"A","kind":"reads","line":2,"to":"C"}],"total_edges":2}`
	right := `{"edges":[{"from":"A","kind":"calls","line":8,"to":"B"},{"from":"A","kind":"reads","line":2,"to":"C"},{"from":"A","kind":"value_flow","line":8,"to":"D"}],"total_edges":3}`
	described := e2eMatrixDescribeDifference(left, right)
	if !strings.Contains(described, "value_flow") {
		t.Fatalf("the differing edge was not named: %s", described)
	}
	if !strings.Contains(described, "only in the fresh index") {
		t.Fatalf("the side that carries it was not named: %s", described)
	}
	if strings.Contains(described, `"kind":"reads"`) {
		t.Fatalf("the shared entries were printed instead of just the difference: %s", described)
	}
	if !strings.Contains(described, "total_edges") {
		t.Fatalf("the scalar that moved was not named: %s", described)
	}
	// The other direction, and the "only on the incremental side" wording,
	// which is the shape that says the incremental view GAINED something.
	if described := e2eMatrixDescribeDifference(right, left); !strings.Contains(described, "only in incremental") {
		t.Fatalf("the incremental-only side was not named: %s", described)
	}
	// A missing key, not a differing one.
	if described := e2eMatrixDescribeDifference(`{"a":1}`, `{"a":1,"b":2}`); !strings.Contains(described, ".b only in fresh") {
		t.Fatalf("a key present on one side only was not named: %s", described)
	}
	// Equal answers describe nothing, and a non-JSON answer still renders.
	if described := e2eMatrixDescribeDifference(left, left); !strings.Contains(described, "compare equal") {
		t.Fatalf("equal answers described as %q", described)
	}
	if described := e2eMatrixDescribeDifference("call error: boom", `{"a":1}`); !strings.Contains(described, "call error: boom") {
		t.Fatalf("a non-JSON answer was swallowed: %s", described)
	}
}

func TestE2EMatrixListMinusIsAMultisetDifference(t *testing.T) {
	left := []any{"a", "b", "b"}
	right := []any{"b", "c"}
	if got := e2eMatrixListMinus(left, right); len(got) != 2 || got[0] != `"a"` || got[1] != `"b"` {
		t.Fatalf("multiset difference %v", got)
	}
	if got := e2eMatrixListMinus(right, left); len(got) != 1 || got[0] != `"c"` {
		t.Fatalf("reverse multiset difference %v", got)
	}
	if got := e2eMatrixListMinus(left, left); len(got) != 0 {
		t.Fatalf("a list differed from itself: %v", got)
	}
}

func TestE2EMatrixSemanticMetadataGapIsNamedAndNarrow(t *testing.T) {
	// The gap hides exactly three keys and nothing else: widening it would
	// blind the gate-1 oracle to real differences.
	gap := e2eMatrixSemanticMetadataKeys()
	if len(gap) != 3 || !gap["semantic_source"] || !gap["semantic_type"] || !gap["return_type"] {
		t.Fatalf("the known-gap key set drifted: %v", gap)
	}
	for key := range e2eMatrixMergedKeys(e2eMatrixVolatileKeys(), e2eMatrixProvenanceKeys()) {
		if gap[key] {
			t.Fatalf("key %q is in two exclusion families; a reader cannot tell which rule hid it", key)
		}
	}
	if strings.TrimSpace(e2eMatrixKnownGapReason) == "" || !strings.Contains(e2eMatrixKnownGapReason, "incremental-build route") {
		t.Fatalf("the skip reason must name what owns the gap: %q", e2eMatrixKnownGapReason)
	}

	// A node that differs ONLY in those keys is explained by the gap; a node
	// that also lost an edge, a location or a signature is not.
	incremental := map[string]any{"nodes": []any{map[string]any{"id": "A", "start_line": 6.0,
		"meta": map[string]any{"signature": "func A() int"}}}}
	fresh := map[string]any{"nodes": []any{map[string]any{"id": "A", "start_line": 6.0,
		"meta": map[string]any{"signature": "func A() int", "semantic_source": "go-types", "semantic_type": "func() int", "return_type": "(int)"}}}}
	structural := e2eMatrixMergedKeys(e2eMatrixVolatileKeys(), e2eMatrixProvenanceKeys())
	residual := e2eMatrixMergedKeys(structural, gap)
	if e2eMatrixCanonical(e2eMatrixNormalize(incremental, "", structural)) == e2eMatrixCanonical(e2eMatrixNormalize(fresh, "", structural)) {
		t.Fatal("the semantic-metadata difference was already invisible to the structural comparison")
	}
	if e2eMatrixCanonical(e2eMatrixNormalize(incremental, "", residual)) != e2eMatrixCanonical(e2eMatrixNormalize(fresh, "", residual)) {
		t.Fatal("the residual comparison did not attribute the difference to the known gap")
	}
	moved := map[string]any{"nodes": []any{map[string]any{"id": "A", "start_line": 7.0,
		"meta": map[string]any{"signature": "func A() int", "semantic_source": "go-types", "semantic_type": "func() int", "return_type": "(int)"}}}}
	if e2eMatrixCanonical(e2eMatrixNormalize(incremental, "", residual)) == e2eMatrixCanonical(e2eMatrixNormalize(moved, "", residual)) {
		t.Fatal("a moved location was excused by the known-gap exclusion")
	}
}

// TestE2EMatrixReuseVerdictRefusesADeadInstrument pins the gate-4 decision: a
// "did not move" reading is reuse only when a step that really did rebuild
// moved the same series. The review found the shipped reading unfalsifiable —
// no dirty build in either matrix ever moved a coordinator_cycle series, so
// "built_dirty did not move" carried no information while the row claimed gate 4
// confirmed.
func TestE2EMatrixReuseVerdictRefusesADeadInstrument(t *testing.T) {
	series := e2eMatrixDirtyBuildSeries()
	key := series[0]

	t.Run("dead instrument is not measurable", func(t *testing.T) {
		text, measurable, reused := e2eMatrixReuseVerdict("undo", map[string]int64{}, map[string]int64{}, series)
		if measurable || reused {
			t.Fatalf("a silent calibration reported measurable=%v reused=%v: %s", measurable, reused, text)
		}
		if !strings.Contains(text, "NOT MEASURABLE") || !strings.Contains(text, key) {
			t.Fatalf("the verdict did not say why it could not be scored: %s", text)
		}
	})

	t.Run("live instrument that did not move is reuse", func(t *testing.T) {
		text, measurable, reused := e2eMatrixReuseVerdict("undo", map[string]int64{key: 1}, map[string]int64{}, series)
		if !measurable || !reused {
			t.Fatalf("a live calibration with a still window reported measurable=%v reused=%v: %s", measurable, reused, text)
		}
		if !strings.Contains(text, "reused") {
			t.Fatalf("the verdict did not report reuse: %s", text)
		}
	})

	t.Run("live instrument that moved is a rebuild", func(t *testing.T) {
		text, measurable, reused := e2eMatrixReuseVerdict("redo", map[string]int64{key: 1}, map[string]int64{key: 1}, series)
		if !measurable || reused {
			t.Fatalf("a moved window reported measurable=%v reused=%v: %s", measurable, reused, text)
		}
		if !strings.Contains(text, "REBUILT") {
			t.Fatalf("the verdict did not report a rebuild: %s", text)
		}
	})

	t.Run("a calibration that moved an unrelated series is still dead", func(t *testing.T) {
		unrelated := map[string]int64{e2eMatrixSeries(viewmetrics.FamilyInventorySeconds): 3}
		if _, measurable, _ := e2eMatrixReuseVerdict("undo", unrelated, map[string]int64{}, series); measurable {
			t.Fatal("movement outside the named series was accepted as calibration")
		}
	})
}

// TestE2EMatrixTrivialAnswerKnowsAnEmptyOrRefusedAnswer pins what does not count as
// evidence: an empty reduction and a transport failure recorded as the answer.
func TestE2EMatrixTrivialAnswerKnowsAnEmptyOrRefusedAnswer(t *testing.T) {
	for _, value := range []string{"", "  ", "[]", "{}", "null",
		"call error: exit status 1: file_not_indexed", "decode error: unexpected end of JSON input",
		"marshal error: unsupported type"} {
		if !e2eMatrixTrivialAnswer(value) {
			t.Fatalf("%q was counted as evidence", value)
		}
	}
	for _, value := range []string{`[{"id":"a"}]`, `{"nodes":[]}`, `{"summary":{"kind":"function"}}`} {
		if e2eMatrixTrivialAnswer(value) {
			t.Fatalf("%q was discarded as carrying no evidence", value)
		}
	}
}

// TestE2EMatrixSubstantiveProbesCountsOnlyRealAgreement is the agreement rule: two empty
// answers, or two error strings, are not a probe that agreed.
func TestE2EMatrixSubstantiveProbesCountsOnlyRealAgreement(t *testing.T) {
	probes := []e2eMatrixProbe{
		{Key: "symbols:Gone", Clause: "resolved target + deletion absence"},
		{Key: "summary:p000/file00000.go", Clause: "locations"},
		{Key: "source:broken", Clause: "source bytes + span"},
		{Key: "usages:one-sided", Clause: "incoming edges"},
	}
	incremental := map[string]string{
		"symbols:Gone":              "[]",
		"summary:p000/file00000.go": `{"kind":"file"}`,
		"source:broken":             "call error: exit status 1",
		"usages:one-sided":          `{"edges":[]}`,
	}
	fresh := map[string]string{
		"symbols:Gone":              "[]",
		"summary:p000/file00000.go": `{"kind":"file"}`,
		"source:broken":             "call error: exit status 1",
	}
	substantive, empty := e2eMatrixSubstantiveProbes(incremental, fresh, probes)
	if substantive != 1 {
		t.Fatalf("substantive = %d, want 1 (only the summary)", substantive)
	}
	if len(empty) != 2 || empty[0] != "source:broken" || empty[1] != "symbols:Gone" {
		t.Fatalf("probes carrying no evidence = %v, want [source:broken symbols:Gone]", empty)
	}
}

// TestE2EMatrixGateOneVerdictKeepsAResidualDifferenceAFailure pins matrix 2's own
// decision. The review's mutation — turning the residual arm into a skip — must
// turn this red.
func TestE2EMatrixGateOneVerdictKeepsAResidualDifferenceAFailure(t *testing.T) {
	gap := []e2eMatrixDifference{{Probe: "summary:p000/file00000.go", Clause: "metadata",
		Incremental: `{"meta":{}}`, Fresh: `{"meta":{"semantic_source":"go-types"}}`}}
	residual := []e2eMatrixDifference{{Probe: "usages:x", Clause: "incoming edges",
		Incremental: `{"edges":[]}`, Fresh: `{"edges":[{"from":"y"}]}`}}

	t.Run("no projection", func(t *testing.T) {
		got := e2eMatrixGateOneVerdict(e2eMatrixOracleInput{Probes: 0})
		if got.Status != e2eMatrixStatusFail || !strings.Contains(strings.Join(got.Failures, " "), "no projection") {
			t.Fatalf("verdict = %+v, want a FAIL naming the missing projection", got)
		}
	})

	t.Run("nothing substantive", func(t *testing.T) {
		got := e2eMatrixGateOneVerdict(e2eMatrixOracleInput{Probes: 2, Substantive: 0, Empty: []string{"symbols:Gone", "source:broken"}})
		if got.Status != e2eMatrixStatusFail {
			t.Fatalf("a comparison of two empty answers = %s, want FAIL: %+v", got.Status, got)
		}
		if !strings.Contains(strings.Join(got.Failures, " "), "agree for free") {
			t.Fatalf("the failure did not say why: %+v", got)
		}
	})

	t.Run("equal", func(t *testing.T) {
		got := e2eMatrixGateOneVerdict(e2eMatrixOracleInput{Probes: 4, Substantive: 3, Empty: []string{"symbols:Gone"}})
		if got.Status != e2eMatrixStatusPass {
			t.Fatalf("verdict = %+v, want PASS", got)
		}
		if !strings.Contains(strings.Join(got.Recorded, " "), "3 substantive") {
			t.Fatalf("the PASS did not report how much of it was substantive: %+v", got)
		}
	})

	t.Run("only the named gap is a named skip", func(t *testing.T) {
		got := e2eMatrixGateOneVerdict(e2eMatrixOracleInput{Probes: 4, Substantive: 3, Differences: gap})
		if got.Status != e2eMatrixStatusSkip {
			t.Fatalf("verdict = %+v, want SKIP", got)
		}
		joined := strings.Join(got.Recorded, " ")
		if !strings.Contains(joined, "incremental-build route") {
			t.Fatalf("the skip did not name what owns the gap: %s", joined)
		}
		for _, wrong := range []string{"Owner: the blame/coverage input channel"} {
			if strings.Contains(joined, wrong) {
				t.Fatalf("the skip still cites a row that does not own the gap (%q): %s", wrong, joined)
			}
		}
	})

	t.Run("a residual difference is a failure", func(t *testing.T) {
		got := e2eMatrixGateOneVerdict(e2eMatrixOracleInput{Probes: 4, Substantive: 3,
			Differences: append(append([]e2eMatrixDifference{}, gap...), residual...),
			Residual:    residual, Attempts: e2eMatrixOracleAttempts, Backoff: time.Minute})
		if got.Status != e2eMatrixStatusFail {
			t.Fatalf("a difference beyond the named gap = %s, want FAIL: %+v", got.Status, got)
		}
		joined := strings.Join(got.Failures, " ")
		if !strings.Contains(joined, "usages:x") || !strings.Contains(joined, "beyond the known semantic-metadata gap") {
			t.Fatalf("the failure did not name the residual probe: %s", joined)
		}
	})
}

// TestE2EMatrixKnownGapReasonNamesTheRowThatOwnsIt pins the attribution the review
// found contradicted by the ledger: the semantic-enrichment pass and the
// enrichment census are `wired` and do not own this gap; the in-memory
// incremental-build route's declared limitation does.
func TestE2EMatrixKnownGapReasonNamesTheRowThatOwnsIt(t *testing.T) {
	if !strings.Contains(e2eMatrixKnownGapReason, "incremental-build route") {
		t.Fatalf("the skip reason does not name what owns the gap: %s", e2eMatrixKnownGapReason)
	}
	if !strings.Contains(e2eMatrixKnownGapReason, "not the closure") {
		t.Fatalf("the skip reason does not quote the limitation it rests on: %s", e2eMatrixKnownGapReason)
	}
	if !strings.Contains(e2eMatrixKnownGapReason, "do NOT own this") {
		t.Fatalf("the skip reason does not retract the rows that do not own the gap: %s", e2eMatrixKnownGapReason)
	}
}

// TestE2EMatrixEditCasesDeclareAComparableProjection pins that every case projects
// something a fresh index can be compared against — the precondition
// e2eMatrixGateOneVerdict refuses at run time.
func TestE2EMatrixEditCasesDeclareAComparableProjection(t *testing.T) {
	// An absence-only effect — the shape file_deleted had when the review
	// found it resting on two empty row lists — can only produce probes that
	// reduce to nothing on both sides. e2eMatrixGateOneVerdict refuses such a
	// comparison at run time (TestE2EMatrixGateOneVerdictKeepsAResidualDifferenceAFailure
	// /nothing_substantive); this pins that the projection really is that thin,
	// so the refusal is the only thing standing between it and a free PASS.
	deleted := e2eMatrixEffect{Absent: []e2eMatrixSighting{{"Gone", "p002/gone.go"}}}
	if probes := e2eMatrixProbesFor(deleted); len(probes) != 1 {
		t.Fatalf("an absence-only effect produced %d probes, want 1", len(probes))
	}
	withControl := e2eMatrixEffect{
		Files:   []string{"p000/file00000.go"},
		Symbols: []e2eMatrixSighting{{"Fn00000S0", "p000/file00000.go"}},
		Absent:  []e2eMatrixSighting{{"Gone", "p002/gone.go"}},
	}
	if probes := e2eMatrixProbesFor(withControl); len(probes) < 4 {
		t.Fatalf("an effect with a control produced %d probes, want at least 4", len(probes))
	}
}

// TestE2EMatrixFileDowngradesAnUnmeasurableClaim pins that a case which asked for a
// claim this instrument cannot carry never files a PASS: the row becomes a
// named SKIP saying which claim could not be scored.
func TestE2EMatrixFileDowngradesAnUnmeasurableClaim(t *testing.T) {
	table := e2eMatrixNewTable(t, "e2eMatrix_file_rule")
	h := &e2eMatrixEditHarness{t: t, table: table}

	h.unmeasurable = nil
	h.file(e2eMatrixRow{Case: "measurable", Gate: "gate4", Status: e2eMatrixStatusPass, Detail: "reused"})

	h.unmeasurable = []string{"undo: NOT MEASURABLE — a step that really did rebuild moved none of [views_coordinator_cycle_total{outcome=built_dirty}]"}
	h.file(e2eMatrixRow{Case: "unmeasurable", Gate: "gate4", Status: e2eMatrixStatusPass, Detail: "reused"})
	h.file(e2eMatrixRow{Case: "already failing", Gate: "gate4", Status: e2eMatrixStatusFail, Detail: "gate1: differs"})

	if len(table.rows) != 3 {
		t.Fatalf("filed %d rows, want 3", len(table.rows))
	}
	if table.rows[0].Status != e2eMatrixStatusPass {
		t.Fatalf("a measurable PASS was downgraded: %+v", table.rows[0])
	}
	if table.rows[1].Status != e2eMatrixStatusSkip {
		t.Fatalf("an unmeasurable claim still filed %s: %+v", table.rows[1].Status, table.rows[1])
	}
	if !strings.Contains(table.rows[1].Detail, "NOT MEASURABLE") {
		t.Fatalf("the skip did not say which claim could not be scored: %+v", table.rows[1])
	}
	if table.rows[2].Status != e2eMatrixStatusFail {
		t.Fatalf("an unmeasurable claim upgraded a FAIL to %s: %+v", table.rows[2].Status, table.rows[2])
	}
	if problems := e2eMatrixTableProblems(table.rows); len(problems) != 0 {
		t.Fatalf("the filed rows are not reportable: %v", problems)
	}
}

// TestE2EMatrixCommitObservationSeriesAreDeclaredAndDisjointFromTheBuildFamily pins
// the vocabulary the unchanged-content row's live clause reads.
//
// A key that is not in the viewmetrics catalog reads zero forever, which would
// turn the one clause that survives the consumer gate into an assertion that
// can only fail. And a series shared with the allocation family would make the
// same counter both "the daemon saw the commit" and "the daemon built": the
// row would then contradict itself on a family that really does publish.
func TestE2EMatrixCommitObservationSeriesAreDeclaredAndDisjointFromTheBuildFamily(t *testing.T) {
	declared := map[string]bool{}
	for _, name := range viewmetrics.SeriesNames() {
		declared[name] = true
	}
	observation := e2eMatrixCommitObservationSeries()
	if len(observation) < 5 {
		t.Fatalf("the commit-observation vocabulary shrank to %d series: %v", len(observation), observation)
	}
	build := map[string]bool{}
	for _, key := range e2eMatrixAllocationSeries() {
		build[key] = true
	}
	seen := map[string]bool{}
	for _, key := range observation {
		if !declared[e2eMatrixSeriesName(key)] {
			t.Fatalf("series %q is not declared in the viewmetrics catalog; the clause would silently read zero", key)
		}
		if strings.Contains(key, "{") && !strings.HasSuffix(key, "}") {
			t.Fatalf("series key %q is malformed", key)
		}
		if build[key] {
			t.Fatalf("series %q is in both the commit-observation and the allocation family", key)
		}
		if seen[key] {
			t.Fatalf("series %q is listed twice", key)
		}
		seen[key] = true
	}
	// The declined publication is the whole point: it is the outcome the
	// consumer gate records, and it is the evidence that the daemon settled
	// the commit without writing anything.
	if !seen[e2eMatrixSeries(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationSkipped)] {
		t.Fatalf("the declined-publication outcome is not in the observation family: %v", observation)
	}
}

// TestE2EMatrixUnchangedCommitVerdictGatesEveryClauseOnItsOwnCalibration is the
// unchanged-content row's regression.
//
// Before the consumer gate, a committed change published a base and an
// unchanged-content commit re-adopted it, so "some claim/replay counter moved"
// was a live assertion. After it, a single-checkout family publishes for
// neither commit — and the ungated assertion failed the daemon for doing
// exactly what the branch promises. Each clause now states which calibration
// it rests on, and a clause with no calibration is recorded or reported as
// unmeasurable rather than scored.
func TestE2EMatrixUnchangedCommitVerdictGatesEveryClauseOnItsOwnCalibration(t *testing.T) {
	observed := func(values ...string) map[string]int64 {
		delta := map[string]int64{}
		for _, key := range values {
			delta[key] = 1
		}
		return delta
	}
	dispatched := e2eMatrixSeries(viewmetrics.DedicatedBaseAdvanceTotal, "outcome="+viewmetrics.AdvanceDispatched)
	skipped := e2eMatrixSeries(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationSkipped)
	built := e2eMatrixSeries(viewmetrics.DedicatedBaseClaimTotal, "outcome="+viewmetrics.DedicatedBaseBuilt)
	published := e2eMatrixSeries(viewmetrics.GenerationPublishedTotal, "owner="+viewmetrics.OwnerCheckout)
	reused := e2eMatrixSeries(viewmetrics.DedicatedBaseClaimTotal, "outcome="+viewmetrics.DedicatedBaseReused)
	join := func(values []string) string { return strings.Join(values, " ;; ") }

	// (1) The shape the consumer gate produces: a real content commit was
	// observed and settled without building, and so was the empty one. The
	// observation clause carries the row; the build and allocation clauses say
	// out loud that they were not asserted.
	failures, recorded, cannot := e2eMatrixUnchangedCommitVerdict(e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        observed(dispatched, skipped),
		Window:             observed(dispatched, skipped),
	})
	if len(failures) != 0 || len(cannot) != 0 {
		t.Fatalf("a commit the daemon observed and declined was scored as a failure: %v %v", failures, cannot)
	}
	for _, want := range []string{"observed and settled", "build clause NOT ASSERTED", "allocation clause NOT ASSERTED"} {
		if !strings.Contains(join(recorded), want) {
			t.Fatalf("the verdict did not record %q: %s", want, join(recorded))
		}
	}

	// (2) The clause that keeps (1) from being vacuous: a commit the daemon
	// never reacted to fails, and the failure names what a real commit moved.
	failures, _, cannot = e2eMatrixUnchangedCommitVerdict(e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        observed(dispatched, skipped),
		Window:             map[string]int64{},
	})
	if len(failures) == 0 || len(cannot) != 0 {
		t.Fatalf("a commit the daemon never saw was not failed: %v %v", failures, cannot)
	}
	if !strings.Contains(join(failures), "reported nothing at all") {
		t.Fatalf("the failure did not name the silence: %s", join(failures))
	}

	// (3) A fixture where even a real content commit moved nothing at all is
	// NOT MEASURABLE, never a pass: the row says which claim it could not
	// score instead of claiming a confirmation.
	failures, _, cannot = e2eMatrixUnchangedCommitVerdict(e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        map[string]int64{},
		Window:             map[string]int64{},
	})
	if len(failures) != 0 {
		t.Fatalf("an uncalibrated instrument failed the daemon: %v", failures)
	}
	if len(cannot) == 0 || !strings.Contains(join(cannot), "cannot tell") {
		t.Fatalf("an uncalibrated observation clause was not reported as unmeasurable: %v", cannot)
	}

	// (4) On a family that DOES publish, the pre-gate strength is unchanged:
	// a real content commit builds, so the unchanged-content commit must show
	// the claim/replay family and must not build or allocate.
	calibration := observed(dispatched, built, published)
	failures, recorded, cannot = e2eMatrixUnchangedCommitVerdict(e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        calibration, CalibrationSeq: 1,
		Window: observed(dispatched, reused), WindowSeq: 0,
	})
	if len(failures) != 0 || len(cannot) != 0 {
		t.Fatalf("a replayed unchanged-content commit was scored as a failure: %v %v", failures, cannot)
	}
	if !strings.Contains(join(recorded), "reuse ["+reused) {
		t.Fatalf("the reuse was not recorded: %s", join(recorded))
	}

	failures, _, _ = e2eMatrixUnchangedCommitVerdict(e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        calibration, CalibrationSeq: 1,
		Window: observed(dispatched), WindowSeq: 0,
	})
	if len(failures) == 0 || !strings.Contains(join(failures), "reported no reuse") {
		t.Fatalf("a publishing family that reported no reuse was not failed: %v", failures)
	}

	failures, _, _ = e2eMatrixUnchangedCommitVerdict(e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        calibration, CalibrationSeq: 1,
		Window: observed(dispatched, built), WindowSeq: 0,
	})
	if len(failures) == 0 || !strings.Contains(join(failures), "built or published") {
		t.Fatalf("an unchanged-content commit that rebuilt was not failed: %v", failures)
	}

	failures, _, _ = e2eMatrixUnchangedCommitVerdict(e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        calibration, CalibrationSeq: 1,
		Window: observed(dispatched, reused), WindowSeq: 1,
	})
	if len(failures) == 0 || !strings.Contains(join(failures), "allocated a generation") {
		t.Fatalf("an unchanged-content commit that allocated a generation was not failed: %v", failures)
	}

	// (5) Unreadable calibration counters are a named skip, never a pass and
	// never a fabricated zero.
	failures, recorded, cannot = e2eMatrixUnchangedCommitVerdict(e2eMatrixUnchangedCommitInput{
		CalibrationErr: "daemon status --format json unavailable",
	})
	if len(failures) != 0 || len(recorded) != 0 || len(cannot) != 1 {
		t.Fatalf("an unreadable calibration was not a single named skip: %v %v %v", failures, recorded, cannot)
	}
	if !strings.Contains(cannot[0], "views counter block") {
		t.Fatalf("the skip did not point at what emits the counters: %s", cannot[0])
	}
}

// TestE2EMatrixUnchangedCommitVerdictScoresAViolationWithoutACalibration is the
// pin on the clause ORDER, which is the whole strength of gate 2 on this row.
//
// The BUILD and ALLOCATION clauses are negative claims: an unchanged-content
// commit must build, publish and allocate nothing. A daemon that does one of
// those has violated gate 2 on any fixture, so the violation must be scored
// before — and independently of — the calibration that only the ABSENCE of a
// violation needs. Scoring the gating first is not a weaker assertion, it is no
// assertion at all: on the fixture this matrix actually runs, whose calibration
// allocates nothing (the consumer gate, dedicated_base_startup.go:774-779),
// every allocating daemon would be scored PASS.
func TestE2EMatrixUnchangedCommitVerdictScoresAViolationWithoutACalibration(t *testing.T) {
	observed := func(values ...string) map[string]int64 {
		delta := map[string]int64{}
		for _, key := range values {
			delta[key] = 1
		}
		return delta
	}
	dispatched := e2eMatrixSeries(viewmetrics.DedicatedBaseAdvanceTotal, "outcome="+viewmetrics.AdvanceDispatched)
	skipped := e2eMatrixSeries(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationSkipped)
	built := e2eMatrixSeries(viewmetrics.DedicatedBaseClaimTotal, "outcome="+viewmetrics.DedicatedBaseBuilt)
	join := func(values []string) string { return strings.Join(values, " ;; ") }

	// The fixture matrix 2 actually runs: a real content commit is observed
	// and declined, builds nothing and allocates nothing.
	consumerGated := e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        observed(dispatched, skipped),
		CalibrationSeq:     0,
	}

	// (1) An allocation over an unchanged tree fails even though the
	// calibration allocated nothing.
	in := consumerGated
	in.Window, in.WindowSeq = observed(dispatched, skipped), 1
	failures, _, cannot := e2eMatrixUnchangedCommitVerdict(in)
	if len(cannot) != 0 {
		t.Fatalf("a scorable violation was reported as unmeasurable: %v", cannot)
	}
	if len(failures) != 1 || !strings.Contains(join(failures), "allocated a generation") {
		t.Fatalf("an unchanged-content commit that allocated a generation on an uncalibrated fixture was not failed: %v", failures)
	}
	if !strings.Contains(join(failures), "on any fixture, calibrated or not") {
		t.Fatalf("the failure did not say why it is sound uncalibrated: %s", join(failures))
	}

	// (2) A build over an unchanged tree fails even though the calibration
	// built nothing.
	in = consumerGated
	in.Window, in.WindowSeq = observed(dispatched, built), 0
	failures, _, cannot = e2eMatrixUnchangedCommitVerdict(in)
	if len(cannot) != 0 {
		t.Fatalf("a scorable violation was reported as unmeasurable: %v", cannot)
	}
	if len(failures) != 1 || !strings.Contains(join(failures), "built or published") {
		t.Fatalf("an unchanged-content commit that built on an uncalibrated fixture was not failed: %v", failures)
	}

	// (3) A calibration commit that carried no content voids every clause that
	// rests on it — but NOT the violations, which still fail.
	void := e2eMatrixUnchangedCommitInput{
		CalibrationContent: false,
		Calibration:        observed(dispatched, built),
		CalibrationSeq:     1,
		Window:             observed(dispatched, built),
		WindowSeq:          1,
	}
	failures, _, cannot = e2eMatrixUnchangedCommitVerdict(void)
	if len(failures) != 2 {
		t.Fatalf("a void calibration suppressed the violations: %v", failures)
	}
	if len(cannot) != 1 || !strings.Contains(join(cannot), "carried no content") {
		t.Fatalf("a void calibration did not make the observation clause unmeasurable: %v", cannot)
	}

	// (4) …and with no violation, a void calibration scores nothing as a pass:
	// both negative clauses say out loud that they were not asserted.
	void.Window, void.WindowSeq = observed(dispatched, skipped), 0
	failures, recorded, cannot := e2eMatrixUnchangedCommitVerdict(void)
	if len(failures) != 0 {
		t.Fatalf("a void calibration failed a clean daemon: %v", failures)
	}
	if len(cannot) != 1 {
		t.Fatalf("a void calibration did not name the unscorable observation clause: %v", cannot)
	}
	for _, want := range []string{"build clause NOT ASSERTED", "allocation clause NOT ASSERTED", "carried no content"} {
		if !strings.Contains(join(recorded), want) {
			t.Fatalf("a void calibration did not record %q: %s", want, join(recorded))
		}
	}
}

// TestE2EMatrixScoreUnchangedCommitRoutesEveryVerdictList pins the hand-off from the
// verdict to the row: failures mark the row FAILED (and come back for the
// caller to report), scored clauses ride on the detail, and a clause the
// fixture cannot score reaches h.unmeasurable — the list file() reads to
// downgrade a would-be PASS to a named SKIP. Dropping any one of the three
// turns a real finding into a green row.
func TestE2EMatrixScoreUnchangedCommitRoutesEveryVerdictList(t *testing.T) {
	observed := func(values ...string) map[string]int64 {
		delta := map[string]int64{}
		for _, key := range values {
			delta[key] = 1
		}
		return delta
	}
	dispatched := e2eMatrixSeries(viewmetrics.DedicatedBaseAdvanceTotal, "outcome="+viewmetrics.AdvanceDispatched)
	skipped := e2eMatrixSeries(viewmetrics.DedicatedBasePublicationTotal, "outcome="+viewmetrics.PublicationSkipped)

	// (1) A violation: the row is FAILED, the detail carries the reason and
	// the measured counters, and the failure comes back to the caller.
	table := e2eMatrixNewTable(t, "e2eMatrix_score_routing")
	h := &e2eMatrixEditHarness{t: t, table: table}
	row := e2eMatrixRow{Case: "commit_unchanged_content", Gate: "gate2+gate4", Status: e2eMatrixStatusPass}
	delta := observed(dispatched, skipped)
	delta[e2eMatrixSeries(viewmetrics.GenerationPublishedTotal, "owner="+viewmetrics.OwnerCheckout)] = 1
	failures := h.scoreUnchangedCommit(&row, e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        observed(dispatched, skipped),
		Window:             delta,
		WindowSeq:          1,
	}, delta)
	if len(failures) == 0 {
		t.Fatalf("a violating window returned no failure to report")
	}
	if row.Status != e2eMatrixStatusFail {
		t.Fatalf("a violating window left the row %s: %+v", row.Status, row)
	}
	for _, want := range []string{"built or published", "allocated a generation", "counters: "} {
		if !strings.Contains(row.Detail, want) {
			t.Fatalf("the failed row did not carry %q: %s", want, row.Detail)
		}
	}
	if len(h.unmeasurable) != 0 {
		t.Fatalf("a scorable violation produced unmeasurable claims: %v", h.unmeasurable)
	}

	// (2) A clean, calibrated window: the row stays a PASS and the scored
	// clauses ride on the detail.
	h = &e2eMatrixEditHarness{t: t, table: table}
	row = e2eMatrixRow{Case: "commit_unchanged_content", Gate: "gate2+gate4", Status: e2eMatrixStatusPass}
	window := observed(dispatched, skipped)
	if failures := h.scoreUnchangedCommit(&row, e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        observed(dispatched, skipped),
		Window:             window,
	}, window); len(failures) != 0 {
		t.Fatalf("a clean window was failed: %v", failures)
	}
	h.file(row)
	if table.rows[len(table.rows)-1].Status != e2eMatrixStatusPass {
		t.Fatalf("a clean, calibrated window filed %s: %+v", table.rows[len(table.rows)-1].Status, table.rows[len(table.rows)-1])
	}
	if !strings.Contains(row.Detail, "observed and settled") {
		t.Fatalf("the scored clause did not reach the row: %s", row.Detail)
	}

	// (3) An unscorable clause reaches h.unmeasurable, and file() turns the
	// would-be PASS into a named SKIP. This is the routing whose deletion
	// would publish a meaningless confirmation as a pass.
	h = &e2eMatrixEditHarness{t: t, table: table}
	row = e2eMatrixRow{Case: "commit_unchanged_content", Gate: "gate2+gate4", Status: e2eMatrixStatusPass}
	if failures := h.scoreUnchangedCommit(&row, e2eMatrixUnchangedCommitInput{
		CalibrationContent: true,
		Calibration:        map[string]int64{},
		Window:             map[string]int64{},
	}, map[string]int64{}); len(failures) != 0 {
		t.Fatalf("an uncalibrated window was failed: %v", failures)
	}
	if len(h.unmeasurable) != 1 {
		t.Fatalf("the unscorable clause did not reach the harness: %v", h.unmeasurable)
	}
	h.file(row)
	filed := table.rows[len(table.rows)-1]
	if filed.Status != e2eMatrixStatusSkip {
		t.Fatalf("an unscorable clause filed %s instead of a named SKIP: %+v", filed.Status, filed)
	}
	if !strings.Contains(filed.Detail, "NOT MEASURABLE") || !strings.Contains(filed.Detail, "cannot tell") {
		t.Fatalf("the skip did not say which claim could not be scored: %s", filed.Detail)
	}

	// (4) An unreadable calibration is a single named skip, routed the same way.
	h = &e2eMatrixEditHarness{t: t, table: table}
	row = e2eMatrixRow{Case: "commit_unchanged_content", Gate: "gate2+gate4", Status: e2eMatrixStatusPass}
	if failures := h.scoreUnchangedCommit(&row, e2eMatrixUnchangedCommitInput{
		CalibrationErr: "daemon status --format json unavailable",
	}, nil); len(failures) != 0 {
		t.Fatalf("an unreadable calibration failed the daemon: %v", failures)
	}
	if len(h.unmeasurable) != 1 || !strings.Contains(h.unmeasurable[0], "views counter block") {
		t.Fatalf("an unreadable calibration was not a single named skip: %v", h.unmeasurable)
	}
	if problems := e2eMatrixTableProblems(table.rows); len(problems) != 0 {
		t.Fatalf("the filed rows are not reportable: %v", problems)
	}
}
