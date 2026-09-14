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

// E2E matrix 2 — the edit taxonomy (handoff section 8, second bullet).
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
// Opt-in through GXW8_TEST_BINARY (see w8_matrix_noop_test.go), private daemon,
// private store, private git. GXW8_MATRIX_ORACLE=0 records every fresh-index
// comparison as a named skip for a fast structural smoke run.

// The oracle re-reads both sides when they disagree: an enrichment pass that
// has not finished on one of them is a converging difference, not a defect, and
// a single sample would report it as one. Three attempts with a minute between
// them bound the cost — each attempt builds another fresh isolated index — while
// giving the asynchronous lanes (language server, go/types semantics) a real
// chance to land on both sides. The budget is a knob because "how long did you
// wait" is part of what a reported difference means.
const (
	w8m5OracleAttempts       = 3
	w8m5DefaultOracleBackoff = time.Minute
)

// w8m5OracleBackoff is how long the oracle waits between two disagreeing
// samples. GXW8_MATRIX_ORACLE_BACKOFF overrides it; the value in force is
// reported on every row that reports a difference.
func w8m5OracleBackoff(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv("GXW8_MATRIX_ORACLE_BACKOFF")
	if raw == "" {
		return w8m5DefaultOracleBackoff
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < 0 || parsed > 10*time.Minute {
		t.Fatalf("GXW8_MATRIX_ORACLE_BACKOFF=%q is not a duration in [0,10m]", raw)
	}
	return parsed
}

// ------------------------------------------------------------- projections ---

// w8m5Probe is one product-surface question asked of both indexes.
type w8m5Probe struct {
	// Key names the probe in the comparison report.
	Key string
	// Clause is the gate-1 vocabulary this probe evidences.
	Clause string
	// Facade and Payload are the CLI call: `gortex call <facade> --json …`.
	Facade  string
	Payload map[string]any
	// Name, when set, reduces the answer to the rows that ARE this symbol
	// (see w8m5ReduceToName). Only the symbol-search probe sets it.
	Name string
}

// w8m5ReduceToName keeps the rows of a search answer that are the named symbol
// and returns them as the whole answer, dropping the page around them.
//
// Why the page itself is not the comparison: `search symbols` answers a RANKED
// PAGE. The generated corpus carries one revision probe per file
// (W8Probe00000Rev0, W8Probe00013Rev0, W8Probe00021Rev0 …), so a query for one
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
func w8m5ReduceToName(value any, name string) any {
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
	sort.Slice(rows, func(i, j int) bool { return w8m5Canonical(rows[i]) < w8m5Canonical(rows[j]) })
	return rows
}

// w8m5VolatileKeys are per-process cache identity, never content.
func w8m5VolatileKeys() map[string]bool {
	return map[string]bool{"etag": true, "fetched_at": true}
}

// w8m5ProvenanceKeys are the asynchronous enrichment axis. They are dropped
// from the asserted structural comparison and compared separately.
func w8m5ProvenanceKeys() map[string]bool {
	return map[string]bool{"origin": true, "tier": true, "confidence_label": true}
}

// w8m5SemanticMetadataKeys is a MEASURED GAP of this branch, not a normalization
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
// filed as a named skip pointing at the ledger row that OWNS the gap — see
// w8m5KnownGapReason for which row that is, and which rows do not. Any
// difference outside these keys stays a hard gate-1 failure, and the full
// difference is recorded in the row either way — a skipped row is a reported
// gap, never a silent pass.
func w8m5SemanticMetadataKeys() map[string]bool {
	return map[string]bool{"semantic_source": true, "semantic_type": true, "return_type": true}
}

// w8m5KnownGapReason is the sentence a skipped row carries.
//
// The attribution was corrected after the adversarial review checked it against
// the ledger. The owner is W6.1b's own declared limitation (2), and the
// mechanism fits exactly: go/types needs a package's other files to type-check
// one of them, and W6.1b's in-memory route runs enrichment "after the pass,
// when the read-only half is already gone", so the closure a rebuilt file needs
// is no longer there — which is why the rebuilt file loses the metadata while
// an untouched file in the same graph keeps it.
//
// The rows the first draft cited do NOT own it and are named here so the ledger
// cannot inherit the mistake: W3.2 is state `wired`
// (docs/incremental-indexing-execution-ledger.md:1213-1216) and W6.11 is
// `wired`, absorbed into W6.1b and committed as 2b76f61a (:3024-3027, :6145),
// its G3 second clause discharged by census. W3.7 is genuinely unstarted
// (:926) but covers the blame / coverage INPUT side channel, not go/types.
const w8m5KnownGapReason = "the branch does not carry go/types semantic metadata (meta.semantic_source / semantic_type / return_type) onto a rebuilt file: the incremental view lacks what a fresh isolated index of the same tree has, while an untouched file in the same graph still carries it. Owner: ledger row W6.1b (absorbing W6.11) declared limitation (2) — 'semantic enrichment now sees the change set, not the closure' — so the go/types pass over a rebuilt file no longer has the package closure it needs. W3.2 and W6.11 are 'wired' and do NOT own this; W3.7 is open but covers the blame/coverage input channel"

func w8m5MergedKeys(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, set := range sets {
		for key := range set {
			out[key] = true
		}
	}
	return out
}

// w8m5Normalize makes one answer comparable across two daemons: the answering
// daemon's own root becomes <ROOT>, the named keys are dropped, and every list
// is ordered by its canonical rendering so ranking cannot masquerade as a
// content difference.
func w8m5Normalize(value any, root string, drop map[string]bool) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			if drop[key] {
				continue
			}
			out[key] = w8m5Normalize(child, root, drop)
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, child := range value {
			out = append(out, w8m5Normalize(child, root, drop))
		}
		sort.Slice(out, func(i, j int) bool { return w8m5Canonical(out[i]) < w8m5Canonical(out[j]) })
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

// w8m5Canonical renders a normalized value deterministically. encoding/json
// sorts map keys, so this is a stable identity for comparison and for diffing.
func w8m5Canonical(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(raw)
}

// w8m5Difference is one probe whose two answers disagree.
type w8m5Difference struct {
	Probe       string `json:"probe"`
	Clause      string `json:"gate1_clause"`
	Incremental string `json:"incremental"`
	Fresh       string `json:"fresh_isolated_index"`
}

// w8m5Compare reports the probes whose normalized answers differ. It is pure so
// the comparison rule is tested without a daemon.
func w8m5Compare(incremental, fresh map[string]string, probes []w8m5Probe) []w8m5Difference {
	var differences []w8m5Difference
	for _, probe := range probes {
		left, hasLeft := incremental[probe.Key]
		right, hasRight := fresh[probe.Key]
		switch {
		case !hasLeft || !hasRight:
			differences = append(differences, w8m5Difference{Probe: probe.Key, Clause: probe.Clause,
				Incremental: w8m5Missing(hasLeft, left), Fresh: w8m5Missing(hasRight, right)})
		case left != right:
			differences = append(differences, w8m5Difference{Probe: probe.Key, Clause: probe.Clause,
				Incremental: left, Fresh: right})
		}
	}
	return differences
}

// w8m5TrivialAnswer reports whether a projected answer carries no evidence of
// its own — an empty reduction, or a transport/decode failure recorded as the
// answer.
//
// Why it matters: w8m5Project stores "call error: …" as the answer so that two
// daemons which both refuse a probe agree rather than the harness inventing a
// difference. That is the right rule for a probe, and the wrong rule for a
// CASE: a case whose whole comparison is two error strings, or two empty row
// lists, reports "N probes equal" while neither index answered anything. The
// review found file_deleted resting on exactly one such probe.
func w8m5TrivialAnswer(value string) bool {
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

// w8m5SubstantiveProbes counts the probes BOTH indexes answered with something,
// and names the ones that carry no evidence. A comparison with no substantive
// probe is refused by w8m5GateOneVerdict.
func w8m5SubstantiveProbes(incremental, fresh map[string]string, probes []w8m5Probe) (substantive int, empty []string) {
	for _, probe := range probes {
		left, hasLeft := incremental[probe.Key]
		right, hasRight := fresh[probe.Key]
		switch {
		case !hasLeft || !hasRight:
			// A probe one side never produced is a difference, not agreement;
			// w8m5Compare reports it and it is neither substantive nor empty.
		case w8m5TrivialAnswer(left) && w8m5TrivialAnswer(right):
			empty = append(empty, probe.Key)
		default:
			substantive++
		}
	}
	sort.Strings(empty)
	return substantive, empty
}

// w8m5OracleResult is what one oracle round produced: the surviving
// differences, how many of them are not the named gap, how many probes differ
// once provenance is included, and how much of the comparison was substantive.
type w8m5OracleResult struct {
	Differences []w8m5Difference
	Residual    []w8m5Difference
	Provenance  int
	Substantive int
	Empty       []string
}

// w8m5OracleInput is everything the gate-1 decision is made from.
type w8m5OracleInput struct {
	Probes      int
	Substantive int
	Empty       []string
	Differences []w8m5Difference
	Residual    []w8m5Difference
	Attempts    int
	Backoff     time.Duration
}

// w8m5GateOneVerdict is matrix 2's decision, separated from the daemon so the
// rule that turns a surviving difference into a FAIL — rather than into the
// named-gap SKIP beside it — is pinned by an ordinary unit test. The review
// found that converting this branch to a skip left the whole suite green.
func w8m5GateOneVerdict(in w8m5OracleInput) w8m5Verdict {
	switch {
	case in.Probes == 0:
		return w8m5Verdict{Status: w8m5StatusFail,
			Failures: []string{"gate1: the case declared no projection to compare"}}
	case in.Substantive == 0:
		return w8m5Verdict{Status: w8m5StatusFail, Failures: []string{fmt.Sprintf(
			"gate1: the comparison rests on no substantive answer — all %d probe(s) are empty or were refused by both daemons (%s), and two indexes that answered nothing agree for free",
			in.Probes, strings.Join(in.Empty, " "))}}
	case len(in.Differences) == 0:
		return w8m5Verdict{Status: w8m5StatusPass, Recorded: []string{fmt.Sprintf(
			"gate1 oracle: %d probes equal (%d substantive, %d carrying no evidence)",
			in.Probes, in.Substantive, len(in.Empty))}}
	case len(in.Residual) == 0:
		return w8m5Verdict{Status: w8m5StatusSkip, Recorded: []string{fmt.Sprintf(
			"gate1: %d probe(s) differ (%d substantive probe(s) compared) and every difference is %s",
			len(in.Differences), in.Substantive, w8m5KnownGapReason)}}
	default:
		rendered := make([]string, 0, len(in.Residual))
		for _, difference := range in.Residual {
			rendered = append(rendered, fmt.Sprintf("%s [%s]: %s", difference.Probe, difference.Clause,
				w8m5DescribeDifference(difference.Incremental, difference.Fresh)))
		}
		return w8m5Verdict{Status: w8m5StatusFail, Failures: []string{fmt.Sprintf(
			"gate1: the incremental view still differs from a fresh isolated index of the same tree after %d samples %s apart, beyond the known semantic-metadata gap: %s",
			in.Attempts, in.Backoff, strings.Join(rendered, " ;; "))}}
	}
}

func w8m5Missing(present bool, value string) string {
	if present {
		return value
	}
	return "<probe not answered>"
}

// w8m5DescribeDifference says WHAT differs between two canonical answers
// instead of printing both of them. Two 4 KB edge lists that differ by one edge
// are unreadable side by side and unactionable truncated; the set difference is
// one line and names the edge.
func w8m5DescribeDifference(left, right string) string {
	var leftValue, rightValue any
	if json.Unmarshal([]byte(left), &leftValue) != nil || json.Unmarshal([]byte(right), &rightValue) != nil {
		return fmt.Sprintf("incremental=%s fresh=%s", w8m5Truncate(left, 300), w8m5Truncate(right, 300))
	}
	parts := w8m5DescribeValue("", leftValue, rightValue)
	if len(parts) == 0 {
		return "the two answers render differently but compare equal"
	}
	return strings.Join(parts, "; ")
}

// w8m5DescribeValue walks two decoded answers in parallel and names the first
// level at which they part company: a missing key, a scalar that moved, or the
// entries that exist on only one side of a list.
func w8m5DescribeValue(path string, left, right any) []string {
	if w8m5Canonical(left) == w8m5Canonical(right) {
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
				parts = append(parts, fmt.Sprintf("%s.%s only in fresh: %s", name, key, w8m5Truncate(w8m5Canonical(rightChild), 200)))
			case !hasRight:
				parts = append(parts, fmt.Sprintf("%s.%s only in incremental: %s", name, key, w8m5Truncate(w8m5Canonical(leftChild), 200)))
			default:
				parts = append(parts, w8m5DescribeValue(path+"."+key, leftChild, rightChild)...)
			}
		}
		return parts
	}
	leftList, leftIsList := left.([]any)
	rightList, rightIsList := right.([]any)
	if leftIsList && rightIsList {
		onlyLeft := w8m5ListMinus(leftList, rightList)
		onlyRight := w8m5ListMinus(rightList, leftList)
		var parts []string
		if len(onlyLeft) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d entr(ies) only in incremental: %s", name, len(onlyLeft), w8m5Truncate(strings.Join(onlyLeft, " "), 600)))
		}
		if len(onlyRight) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d entr(ies) only in the fresh index: %s", name, len(onlyRight), w8m5Truncate(strings.Join(onlyRight, " "), 600)))
		}
		if len(parts) == 0 {
			parts = append(parts, fmt.Sprintf("%s: same entries in a different multiplicity (%d vs %d)", name, len(leftList), len(rightList)))
		}
		return parts
	}
	return []string{fmt.Sprintf("%s: incremental=%s fresh=%s", name,
		w8m5Truncate(w8m5Canonical(left), 200), w8m5Truncate(w8m5Canonical(right), 200))}
}

// w8m5ListMinus is the multiset difference of two decoded lists, rendered.
func w8m5ListMinus(left, right []any) []string {
	counts := map[string]int{}
	for _, item := range right {
		counts[w8m5Canonical(item)]++
	}
	var only []string
	for _, item := range left {
		key := w8m5Canonical(item)
		if counts[key] > 0 {
			counts[key]--
			continue
		}
		only = append(only, key)
	}
	return only
}

// --------------------------------------------------------------- the cases ---

// w8m5Sighting is one "this name must (not) answer out of this file" fact.
type w8m5Sighting struct {
	Name string
	Rel  string // repo-relative, slash-separated
}

// w8m5Effect is what a case changed and what has to be probed afterwards.
type w8m5Effect struct {
	Files   []string       // repo-relative files to project (must exist after the case)
	Symbols []w8m5Sighting // symbols to project through usages/source
	Present []w8m5Sighting // names that must answer from their file
	Absent  []w8m5Sighting // names that must NOT answer (deletion absence)
	// Observe are names the oracle projects without the case asserting
	// anything about them. They are how a case asks a question whose answer
	// is a product behaviour rather than a gate: "is a gitignored file
	// indexed at all?" is not something gate 1 decides — gate 1 decides
	// whether the composed view and a fresh isolated index of the same tree
	// AGREE about it. The case records which way the branch answered, and the
	// oracle still compares the name on both sides.
	Observe []w8m5Sighting
	Texts   []string // literals for the text lane
	Note    string
}

// w8m5EditCase is one row of matrix 2.
type w8m5EditCase struct {
	Name  string
	Gate  string
	Apply func(h *w8m5EditHarness) w8m5Effect
}

// w8m5EditCaseNames is the handoff's own vocabulary for this matrix. The case
// table is pinned to it so a class cannot quietly leave the taxonomy.
func w8m5EditCaseNames() []string {
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

// w8m5EditCases is matrix 2, in the order the handoff lists the classes.
func w8m5EditCases() []w8m5EditCase {
	return []w8m5EditCase{
		{
			Name: "comment_only", Gate: "gate1+gate3",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel := h.rel(0)
				h.rewrite(rel, func(source string) string {
					return "// w8m5: a comment-only change carries no declaration.\n" + source
				})
				h.commit("comment only")
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{h.leaf(0), rel}},
					Present: []w8m5Sighting{{h.probe(0), rel}},
					Texts:   []string{"a comment-only change carries no declaration"},
					Note:    "a comment line added above the package clause",
				}
			},
		},
		{
			Name: "presentation_only", Gate: "gate1+gate3",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel := h.rel(1)
				h.rewrite(rel, func(source string) string {
					// One more blank line wherever there was one: every byte
					// offset and every line number below the first paragraph
					// moves, and no declaration does.
					return strings.ReplaceAll(source, "\n\n", "\n\n\n")
				})
				h.commit("presentation only")
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{h.leaf(1), rel}},
					Present: []w8m5Sighting{{h.probe(1), rel}},
					Note:    "vertical whitespace only; every declaration keeps its name and its order",
				}
			},
		},
		{
			Name: "directive_added", Gate: "gate1+gate3",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel := h.rel(2)
				h.rewrite(rel, func(source string) string {
					return strings.Replace(source, "\nfunc ", "\n//go:generate echo w8m5\nfunc ", 1)
				})
				h.commit("directive added")
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{h.leaf(2), rel}},
					Present: []w8m5Sighting{{h.probe(2), rel}},
					Texts:   []string{"go:generate echo w8m5"},
					Note:    "a //go:generate directive above the first function",
				}
			},
		},
		{
			Name: "body_change", Gate: "gate1+gate4",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel := h.rel(3)
				index := h.index(3)
				h.rewriteFile(rel, func(source string) string {
					edited, err := w8EditFileSource(source, index, 1)
					if err != nil {
						h.t.Fatal(err)
					}
					return edited
				})
				h.commit("body change")
				h.revision[index] = 1
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{h.leaf(3), rel}},
					Present: []w8m5Sighting{{w8ProbeName(index, 1), rel}},
					Absent:  []w8m5Sighting{{w8ProbeName(index, 0), rel}},
					Note:    "one function body and the revision stub",
				}
			},
		},
		{
			Name: "signature_change", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel, index := h.rel(4), h.index(4)
				leaf := h.leaf(4)
				h.rewrite(rel, func(source string) string {
					// The leaf gains a parameter; its same-file callers are
					// updated, so the edit is a real signature change with a
					// real call-site rewrite rather than a broken file.
					source = strings.Replace(source,
						fmt.Sprintf("func %s() int { return ", leaf),
						fmt.Sprintf("func %s(w8m5Bias int) int { return w8m5Bias + ", leaf), 1)
					return strings.ReplaceAll(source, leaf+"()", leaf+"(1)")
				})
				h.commit("signature change")
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{leaf, rel}},
					Present: []w8m5Sighting{{w8ProbeName(index, h.revision[index]), rel}, {leaf, rel}},
					Texts:   []string{"w8m5Bias"},
					Note:    "a parameter added to the leaf function and every same-file call site rewritten",
				}
			},
		},
		{
			Name: "export_change", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel := h.rel(5)
				old := h.leaf(5)
				renamed := "w8m5Unexported" + strings.TrimPrefix(old, "Fn")
				h.rewrite(rel, func(source string) string { return strings.ReplaceAll(source, old, renamed) })
				h.commit("export change")
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{renamed, rel}},
					Present: []w8m5Sighting{{renamed, rel}},
					Absent:  []w8m5Sighting{{old, rel}},
					Note:    "an exported leaf made unexported; visibility and every same-file edge move with it",
				}
			},
		},
		{
			Name: "import_change", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel := h.rel(6)
				// A new file in package p000 that imports p001 and calls into
				// it: a new cross-package import and a new resolved edge.
				importer := "p000/w8m5_import.go"
				h.write(importer, "package p000\n\nimport w8m5cross \""+w8Module+"/p001\"\n\n"+
					"// W8m5ImportedCaller is the import-change probe.\nfunc W8m5ImportedCaller() int { return w8m5cross."+h.leafFor(1)+"() }\n")
				h.commit("import change")
				return w8m5Effect{
					Files: []string{importer, "p001/" + filepath.Base(h.relFor(1))}, Symbols: []w8m5Sighting{{"W8m5ImportedCaller", importer}},
					Present: []w8m5Sighting{{"W8m5ImportedCaller", importer}, {h.probe(6), rel}},
					Texts:   []string{"W8m5ImportedCaller"},
					Note:    "a new file adding a cross-package import and a resolved cross-package call",
				}
			},
		},
		{
			Name: "file_added", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				added := "p002/w8m5_added.go"
				h.write(added, "package p002\n\n// W8m5AddedSymbol is the added-file probe.\nfunc W8m5AddedSymbol() int { return 8005 }\n")
				h.commit("file added")
				return w8m5Effect{
					Files: []string{added}, Symbols: []w8m5Sighting{{"W8m5AddedSymbol", added}},
					Present: []w8m5Sighting{{"W8m5AddedSymbol", added}},
					Texts:   []string{"W8m5AddedSymbol"},
					Note:    "one new file in an existing package",
				}
			},
		},
		{
			Name: "file_deleted", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				deleted := "p002/w8m5_added.go"
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
				return w8m5Effect{
					Files:   []string{control},
					Symbols: []w8m5Sighting{{h.leaf(10), control}},
					Present: []w8m5Sighting{{h.probe(10), control}},
					Absent:  []w8m5Sighting{{"W8m5AddedSymbol", deleted}},
					Note:    "the added file removed again; deletion absence is the assertion, beside an untouched control file so the comparison is not two empty answers",
				}
			},
		},
		{
			Name: "file_renamed", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				from, to := "p000/w8m5_import.go", "p000/w8m5_renamed.go"
				h.git("mv", filepath.FromSlash(from), filepath.FromSlash(to))
				h.commit("file renamed")
				return w8m5Effect{
					Files: []string{to}, Symbols: []w8m5Sighting{{"W8m5ImportedCaller", to}},
					Present: []w8m5Sighting{{"W8m5ImportedCaller", to}},
					Absent:  []w8m5Sighting{{"W8m5ImportedCaller", from}},
					Note:    "the same declaration under a new path: the old location must be gone",
				}
			},
		},
		{
			Name: "mode_change", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel := h.rel(7)
				path := h.path(rel)
				if err := os.Chmod(path, 0o700); err != nil {
					h.t.Fatal(err)
				}
				h.commit("mode change")
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{h.leaf(7), rel}},
					Present: []w8m5Sighting{{h.probe(7), rel}},
					Note:    "the file mode changed and not one byte of content",
				}
			},
		},
		{
			Name: "symlink_added", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				rel := h.rel(8)
				// The link deliberately does not end in .go: a second path to
				// the same Go file inside the same package would duplicate
				// every declaration in it, and the case would then be about
				// duplicate declarations rather than about symlinks.
				link := "p000/w8m5_link.txt"
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
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{h.leaf(8), rel}},
					Present: []w8m5Sighting{{h.probe(8), rel}},
					Note:    "a symlink pointing at an indexed file; both indexes must reach the same verdict about it",
				}
			},
		},
		{
			Name: "untracked_path", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				ignored := "w8m5_ignored/ignored.go"
				// Order matters for what this case can be said to show, and
				// the review corrected the first description of it: the
				// .gitignore is written BEFORE the file, so the file is
				// ignored from the moment it appears. Nothing is "retained"
				// here — the question is whether a path that was ignored
				// before it ever existed is indexed, and whether the composed
				// view and a fresh isolated index of the same tree AGREE about
				// it. (The "already-indexed content withdrawn by a newly
				// committed exclusion" shape is configuration_change, below.)
				h.write(".gitignore", "w8m5_ignored/\n")
				h.write(ignored, "package w8m5ignored\n\n// W8m5IgnoredSymbol is the untracked-path probe.\nfunc W8m5IgnoredSymbol() int { return 1 }\n")
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
				h.observe("the ignored, never-committed file", "W8m5IgnoredSymbol", ignored)
				return w8m5Effect{
					Files:   []string{h.rel(0)},
					Observe: []w8m5Sighting{{"W8m5IgnoredSymbol", ignored}},
					Note:    "a path ignored before it ever existed, present in the working tree",
				}
			},
		},
		{
			Name: "edit_undo_redo", Gate: "gate4",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
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
				// (w8m5ReuseVerdict, and the harness's unmeasurable list).
				h.rebaseline()
				h.rewriteFile(rel, func(string) string { return h.edited(original, index, 1) })
				h.waitPresent(w8ProbeName(index, 1), rel)
				h.settle()
				calibration, calibrationErr := h.deltaSince()
				if calibrationErr != nil {
					h.note("the gate-4 reuse instrument could not be calibrated: " + calibrationErr.Error())
				} else {
					h.note("gate-4 calibration — a real dirty rebuild moved [" +
						strings.Join(w8m5MovedSeries(calibration, w8m5DirtyBuildSeries()), " ") + "]")
				}

				h.rebaseline()
				h.rewriteFile(rel, func(string) string { return original })
				h.waitPresent(w8ProbeName(index, 0), rel)
				h.settle()
				undo := h.reuseSince("undo to the byte-identical original", calibration, w8m5DirtyBuildSeries())

				h.rebaseline()
				h.rewriteFile(rel, func(string) string { return h.edited(original, index, 1) })
				h.waitPresent(w8ProbeName(index, 1), rel)
				h.settle()
				redo := h.reuseSince("redo of the already-seen edit", calibration, w8m5DirtyBuildSeries())

				h.revision[index] = 1
				return w8m5Effect{
					Files: []string{rel}, Symbols: []w8m5Sighting{{h.leaf(9), rel}},
					Present: []w8m5Sighting{{w8ProbeName(index, 1), rel}},
					Absent:  []w8m5Sighting{{w8ProbeName(index, 0), rel}},
					Note:    "dirty edit, undo to the byte-identical original, redo | " + undo + " | " + redo,
				}
			},
		},
		{
			Name: "commit_unchanged_content", Gate: "gate2+gate4",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				// Commit whatever the previous case left dirty, then commit
				// again over an unchanged tree: the second commit is the
				// replay the gate names.
				h.commit("land the pending edits")
				h.settle()
				// Landing the previous case's dirty edit legitimately
				// publishes; the measured window is the commit AFTER it.
				h.rebaseline()
				h.git("commit", "--allow-empty", "-m", "unchanged content")
				return w8m5Effect{
					Files:   []string{h.rel(9)},
					Present: []w8m5Sighting{{w8ProbeName(h.index(9), h.revision[h.index(9)]), h.rel(9)}},
					Note:    "a commit that changes no content at all",
				}
			},
		},
		{
			Name: "manifest_only_change", Gate: "gate1+gate3",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
				h.rewrite("go.mod", func(source string) string {
					return source + "\n// w8m5: manifest-only change\n"
				})
				h.commit("manifest only")
				return w8m5Effect{
					Files: []string{h.rel(0), "go.mod"}, Symbols: []w8m5Sighting{{h.leaf(0), h.rel(0)}},
					Present: []w8m5Sighting{{h.probe(0), h.rel(0)}},
					Note:    "the module manifest changed and no source file did",
				}
			},
		},
		{
			Name: "configuration_change", Gate: "gate1",
			Apply: func(h *w8m5EditHarness) w8m5Effect {
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
				h.observe("the excluded package", h.leafFor(3), excluded)
				return w8m5Effect{
					Files:   []string{h.rel(0)},
					Observe: []w8m5Sighting{{h.leafFor(3), excluded}},
					Present: []w8m5Sighting{{h.probe(0), h.rel(0)}},
					Note:    "an exclude added to the repository's own configuration",
				}
			},
		},
	}
}

// ---------------------------------------------------------------- harness ---

type w8m5EditHarness struct {
	t       *testing.T
	f       *issue767Fixture
	db      *sql.DB
	table   *w8m5Table
	binary  string
	spec    w8FixtureSpec
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
	baseline     w8m5Catalog
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
}

func (h *w8m5EditHarness) index(slot int) int { return h.rotation[slot%len(h.rotation)] }

func (h *w8m5EditHarness) relFor(index int) string {
	return w8FilePath(index%h.spec.Packages, index)
}

func (h *w8m5EditHarness) rel(slot int) string { return h.relFor(h.index(slot)) }

func (h *w8m5EditHarness) leafFor(index int) string { return fmt.Sprintf("Fn%05dS0", index) }

func (h *w8m5EditHarness) leaf(slot int) string { return h.leafFor(h.index(slot)) }

func (h *w8m5EditHarness) probe(slot int) string {
	index := h.index(slot)
	return w8ProbeName(index, h.revision[index])
}

func (h *w8m5EditHarness) path(rel string) string {
	return filepath.Join(h.f.primary, filepath.FromSlash(rel))
}

func (h *w8m5EditHarness) read(rel string) string {
	h.t.Helper()
	source, err := os.ReadFile(h.path(rel))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(source)
}

func (h *w8m5EditHarness) write(rel, content string) {
	h.t.Helper()
	h.f.write(h.path(rel), content)
}

// rewrite applies a transformation and refuses a no-change rewrite: a case that
// silently edited nothing would pass its own oracle comparison for free.
func (h *w8m5EditHarness) rewrite(rel string, edit func(string) string) {
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
func (h *w8m5EditHarness) rewriteFile(rel string, edit func(string) string) {
	h.t.Helper()
	h.write(rel, edit(h.read(rel)))
}

func (h *w8m5EditHarness) edited(source string, index, revision int) string {
	h.t.Helper()
	out, err := w8EditFileSource(source, index, revision)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *w8m5EditHarness) git(args ...string) {
	h.t.Helper()
	h.f.git(h.f.primary, args...)
}

// commit stages everything and commits; an empty tree difference is allowed so
// a case that only changed a file mode or nothing at all still produces a
// commit.
func (h *w8m5EditHarness) commit(message string) {
	h.t.Helper()
	h.git("add", "-A")
	h.git("commit", "--allow-empty", "-m", "w8m5: "+message)
}

// settle waits for the daemon to go quiet, without failing anything: a busy
// host that keeps a generation counter moving has not, by that fact, broken a
// gate. Non-convergence is recorded on the row instead (see the soft-wait note
// in w8_matrix_noop_test.go).
func (h *w8m5EditHarness) settle() {
	if !w8m5SoftSettle(h.t.Context(), h.db, 2*time.Minute) {
		h.note("the daemon never produced three stable generation samples within 2m: this step's observation was taken over a moving target")
	}
}

// observe records what the daemon currently says about one name and one file,
// without asserting it. It is how a case states a product-behaviour question
// whose answer is not a gate (see w8m5Effect.Observe): the row carries the
// branch's answer, and the gate-1 oracle still compares that name across both
// indexes.
func (h *w8m5EditHarness) observe(label, name, rel string) {
	if w8m5AbsentFrom(h.f, name, h.path(rel)) {
		h.note(label + ": not served — " + name + " does not answer from " + rel)
		return
	}
	h.note(label + ": " + w8m5AbsenceEvidence(h.f, name, h.path(rel)))
}

// note records something a reader has to know about this row without failing it.
func (h *w8m5EditHarness) note(text string) {
	h.notes = append(h.notes, text)
}

// unmet records a step the daemon never carried out. The case keeps running —
// later steps still exercise the daemon and are still worth recording — but its
// row is failed with this reason and the gate-1 oracle is skipped: comparing a
// view that never reached the state the case asked for would report a
// difference the harness invented rather than one the branch produced.
func (h *w8m5EditHarness) unmet(format string, args ...any) {
	h.unmetSteps = append(h.unmetSteps, fmt.Sprintf(format, args...))
}

// waitPresent and waitAbsent are the fixture's symbol waits made non-fatal, so
// an unmet wait fails one row instead of ending the matrix.
func (h *w8m5EditHarness) waitPresent(name, rel string) {
	h.t.Helper()
	ok := w8m5SoftAwait(h.t.Context(), w8m5ProbeTimeout, 500*time.Millisecond, func() bool {
		found, err := h.f.trySearchSymbolIn(h.f.primary, name, h.path(rel))
		return err == nil && found
	})
	if !ok {
		h.unmet("%s never answered from %s within %s", name, rel, w8m5ProbeTimeout)
	}
}

func (h *w8m5EditHarness) waitAbsent(name, rel string) {
	h.t.Helper()
	ok := w8m5SoftAwait(h.t.Context(), w8m5AbsenceTimeout, 500*time.Millisecond, func() bool {
		return w8m5AbsentFrom(h.f, name, h.path(rel))
	})
	if !ok {
		h.unmet("%s was never withdrawn from %s within %s: %s", name, rel, w8m5AbsenceTimeout,
			w8m5AbsenceEvidence(h.f, name, h.path(rel)))
	}
}

func (h *w8m5EditHarness) counters() (map[string]int64, error) {
	output, err := h.f.tryCommand(w8m5CommandTimeout, h.f.primary, "daemon", "status", "--format", "json", "--no-progress")
	if err != nil {
		return nil, fmt.Errorf("daemon status --format json unavailable: %w", err)
	}
	return w8ParseStatusCounters(output)
}

func (h *w8m5EditHarness) resetCounters() { h.countersBase, h.countersErr = h.counters() }

// rebaseline re-opens the measurement window: both the catalog observation and
// the counter snapshot the closing half subtracts from. A case calls it when
// its own setup had to publish something before the behaviour it measures.
func (h *w8m5EditHarness) rebaseline() {
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
func (h *w8m5EditHarness) swapT(t *testing.T) func() {
	previous, previousFixture := h.t, h.f.t
	h.t, h.f.t = t, t
	return func() { h.t, h.f.t = previous, previousFixture }
}

// w8m5DirtyBuildSeries is the series that says a working-tree layer was built
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
// read straight off this series: w8m5ReuseVerdict requires a CALIBRATION — the
// same series over a step that really did rebuild — and, when the calibration
// shows the series stayed still there too, reports the claim as NOT MEASURABLE
// instead of as reuse.
func w8m5DirtyBuildSeries() []string {
	return []string{w8m5Series(viewmetrics.CoordinatorCycleTotal, "outcome="+viewmetrics.OutcomeBuiltDirty)}
}

// w8m5ReuseVerdict decides what "the build series did not move" means, given
// what a step that really did rebuild did to the SAME series.
//
// It is pure so the decision is pinned without a daemon: calibration silent →
// not measurable; calibration moved and the window moved → rebuilt; calibration
// moved and the window did not → reused.
func w8m5ReuseVerdict(label string, calibration, window map[string]int64, series []string) (text string, measurable, reused bool) {
	movedInCalibration := w8m5MovedSeries(calibration, series)
	movedInWindow := w8m5MovedSeries(window, series)
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
func (h *w8m5EditHarness) deltaSince() (map[string]int64, error) {
	h.t.Helper()
	after, err := h.counters()
	if h.countersErr != nil {
		return nil, h.countersErr
	}
	if err != nil {
		return nil, err
	}
	return w8CounterDelta(h.countersBase, after), nil
}

// reuseSince closes a sub-window and scores a gate-4 reuse claim against a
// calibration window taken over a step that really did rebuild. It is how a
// case scopes a gate-4 claim to one step of a multi-step body — the undo half
// of an edit/undo/redo round trip is a reuse claim, the edit before it is not.
//
// Unreadable counters are a named skip sentence pointing at the ledger row that
// emits them, never a silent pass and never a fabricated zero. A claim the
// instrument cannot carry is recorded as unmeasurable, which turns the row into
// a named SKIP rather than leaving a "confirmed" that means nothing.
func (h *w8m5EditHarness) reuseSince(label string, calibration map[string]int64, forbidden []string) string {
	h.t.Helper()
	delta, err := h.deltaSince()
	if err != nil {
		reason := label + ": not scored — daemon status views counters unavailable (ledger row W8.3 emits them): " + err.Error()
		h.unmeasurable = append(h.unmeasurable, reason)
		return reason
	}
	text, measurable, reused := w8m5ReuseVerdict(label, calibration, delta, forbidden)
	switch {
	case !measurable:
		h.unmeasurable = append(h.unmeasurable, text)
	case !reused:
		h.t.Errorf("gate4: %s rebuilt instead of reusing the retained layer: %s", label, text)
	}
	return text
}

func (h *w8m5EditHarness) catalog() w8m5Catalog {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return w8m5ReadCatalog(ctx, h.db)
}

// ----------------------------------------------------------- the oracle ---

// w8m5RootToken is the placeholder a probe payload carries where an absolute
// path belongs. Each daemon is asked with the token replaced by ITS OWN root:
// the two indexes live under different roots, and a payload built from one
// daemon's root is answered by the other with "file_not_indexed" — a difference
// the harness would have invented rather than observed.
const w8m5RootToken = "<ROOT>"

// w8m5ProbesFor turns a case's effect into the projection set. Every probe
// names the gate-1 clause it evidences.
func w8m5ProbesFor(effect w8m5Effect) []w8m5Probe {
	var probes []w8m5Probe
	for _, rel := range effect.Files {
		probes = append(probes, w8m5Probe{
			Key: "summary:" + rel, Clause: "locations+metadata+edges+ownership+visibility",
			Facade:  "read",
			Payload: map[string]any{"operation": "summary", "path": w8m5RootToken + string(filepath.Separator) + filepath.FromSlash(rel)},
		})
	}
	for _, symbol := range effect.Symbols {
		id := w8m5SymbolID(symbol.Rel, symbol.Name)
		probes = append(probes,
			w8m5Probe{Key: "usages:" + id, Clause: "incoming edges",
				Facade:  "relations",
				Payload: map[string]any{"operation": "usages", "target": map[string]any{"symbol": id}}},
			w8m5Probe{Key: "source:" + id, Clause: "source bytes + span",
				Facade:  "read",
				Payload: map[string]any{"operation": "source", "target": map[string]any{"symbol": id}}},
		)
	}
	named := append([]w8m5Sighting{}, effect.Present...)
	named = append(named, effect.Absent...)
	named = append(named, effect.Observe...)
	for _, sighting := range named {
		probes = append(probes, w8m5Probe{
			Key: "symbols:" + sighting.Name, Clause: "resolved target + deletion absence",
			Facade: "search", Name: sighting.Name,
			Payload: map[string]any{"operation": "symbols", "query": sighting.Name,
				"options": map[string]any{"limit": 25, "query_class": "symbol", "expand": "off"}},
		})
	}
	for _, text := range effect.Texts {
		probes = append(probes, w8m5Probe{
			Key: "text:" + text, Clause: "text lane",
			Facade:  "search",
			Payload: map[string]any{"operation": "text", "query": text},
		})
	}
	return w8m5DedupeProbes(probes)
}

func w8m5DedupeProbes(probes []w8m5Probe) []w8m5Probe {
	seen := map[string]bool{}
	out := make([]w8m5Probe, 0, len(probes))
	for _, probe := range probes {
		if seen[probe.Key] {
			continue
		}
		seen[probe.Key] = true
		out = append(out, probe)
	}
	return out
}

// w8m5SymbolID is the graph identity of a declaration, as the product spells
// it: "<repo prefix>/<repo-relative path>::<name>".
func w8m5SymbolID(rel, name string) string {
	return issue767FixturePrefix + "/" + rel + "::" + name
}

// w8m5Project asks one daemon every probe and returns the canonical rendering
// of each answer. A transport failure is recorded as the answer rather than
// failing the test: two daemons that both refuse a probe agree, and a daemon
// that refuses one the other answered is exactly the difference to report.
func w8m5Project(f *issue767Fixture, probes []w8m5Probe, drop map[string]bool) map[string]string {
	out := make(map[string]string, len(probes))
	for _, probe := range probes {
		payload, err := json.Marshal(probe.Payload)
		if err != nil {
			out[probe.Key] = "marshal error: " + err.Error()
			continue
		}
		// The payload is written against <ROOT>; every daemon is asked about
		// its own checkout.
		request := strings.ReplaceAll(string(payload), w8m5RootToken, f.primary)
		output, err := f.tryCommand(w8m5CommandTimeout, f.primary, "call", probe.Facade,
			"--index", f.primary, "--json", request, "--format", "json")
		if err != nil {
			out[probe.Key] = "call error: " + err.Error() + ": " + w8Tail(output)
			continue
		}
		var value any
		if err := json.Unmarshal(output, &value); err != nil {
			out[probe.Key] = "decode error: " + err.Error() + ": " + w8Tail(output)
			continue
		}
		normalized := w8m5Normalize(value, f.primary, drop)
		if probe.Name != "" {
			normalized = w8m5ReduceToName(normalized, probe.Name)
		}
		out[probe.Key] = w8m5Canonical(normalized)
	}
	return out
}

// w8m5CopyTree copies a checkout's working tree — the exact bytes the
// incremental daemon is serving — into a fresh fixture's root. The Git
// directory is deliberately not copied: the oracle commits the same content
// under its own history, and gate 1 compares the index of a tree, not the
// commit that produced it.
func w8m5CopyTree(t *testing.T, from, to string) {
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

// w8m5FreshIndex starts a throwaway isolated daemon over a copy of the tree and
// returns it ready. The caller stops it as soon as the comparison is done: one
// extra daemon at a time, never seventeen.
func w8m5FreshIndex(t *testing.T, binary, tree string) *issue767Fixture {
	t.Helper()
	fresh := newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		w8m5CopyTree(t, tree, f.primary)
	})
	fresh.start()
	fresh.awaitSymbolIn(fresh.primary, w8PrimaryMarker, filepath.Join(fresh.primary, "marker.go"), w8m5ProbeTimeout)
	// A fresh index that has not gone quiet is still sampled: the oracle
	// re-samples on difference, so a converging index costs a retry rather
	// than a wrong verdict, while a hard failure here would cost the case.
	if !w8m5SoftSettle(t.Context(), fresh.openReadOnly(), 2*time.Minute) {
		t.Logf("the fresh isolated index did not produce three stable generation samples within 2m; sampling it anyway")
	}
	return fresh
}

// run executes one case, files its row and — unless the oracle is switched off
// — compares the incremental view against a fresh isolated index of the tree
// the case produced.
func (h *w8m5EditHarness) run(c w8m5EditCase) {
	h.t.Helper()
	started := time.Now()
	h.notes, h.unmetSteps, h.unmeasurable = nil, nil, nil
	h.rebaseline()
	row := w8m5Row{Case: c.Name, Gate: c.Gate, Status: w8m5StatusPass}

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
	row.Bookkeeping = w8m5BookkeepingDelta(before, after)
	row.Detail = effect.Note

	// Gate 4: what did this case cost — reuse or rebuild? Recorded for every
	// case from the daemon's own counters; the replay case asserts on it.
	// A case that asserts on a series it could not read is a named skip
	// pointing at the ledger row that emits it, never a silent pass.
	assertsReuse := c.Name == "commit_unchanged_content"
	afterCounters, err := h.counters()
	switch {
	case h.countersErr != nil || err != nil:
		reason := "daemon status views counters unavailable (ledger row W8.3 emits them)"
		row.Detail = strings.TrimSpace(row.Detail + " | " + reason)
		if assertsReuse {
			row.Status = w8m5StatusSkip
			row.Detail = reason + "; the gate-4 reuse assertion could not run"
			h.file(row)
			return
		}
	default:
		delta := w8CounterDelta(h.countersBase, afterCounters)
		row.Counters = delta
		built := w8m5MovedSeries(delta, w8m5AllocationSeries())
		replayed := w8m5MovedSeries(delta, w8m5ReplaySeries())
		row.Detail = strings.TrimSpace(row.Detail + fmt.Sprintf(" | rebuild[%s] reuse[%s] seq %+d",
			strings.Join(built, " "), strings.Join(replayed, " "), after.Sequence-before.Sequence))
		if assertsReuse {
			if len(replayed) == 0 {
				h.fail(&row, "gate4: an unchanged-content commit reported no reuse; counters: "+w8m5Truncate(w8m5Canonical(delta), 300))
			}
			if after.Sequence != before.Sequence {
				h.fail(&row, fmt.Sprintf("gate2: an unchanged-content commit allocated a generation (seq %+d)", after.Sequence-before.Sequence))
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
		row.Status = w8m5StatusSkip
		row.Detail = "fresh-index oracle disabled by GXW8_MATRIX_ORACLE=0; " + row.Detail
		h.file(row)
		return
	}

	probes := w8m5ProbesFor(effect)
	drop := w8m5MergedKeys(w8m5VolatileKeys(), w8m5ProvenanceKeys())
	result := h.compareAgainstFreshIndex(probes, drop)
	differences, residual, provenance := result.Differences, result.Residual, result.Provenance
	row.Differences = differences
	verdict := w8m5GateOneVerdict(w8m5OracleInput{
		Probes: len(probes), Substantive: result.Substantive, Empty: result.Empty,
		Differences: differences, Residual: residual,
		Attempts: w8m5OracleAttempts, Backoff: h.backoff,
	})
	switch {
	case verdict.Status == w8m5StatusPass:
		row.Detail = strings.TrimSpace(row.Detail + " | " + strings.Join(verdict.Recorded, " | "))
	case verdict.Status == w8m5StatusSkip:
		// Every difference is the measured semantic-metadata gap. The row is
		// a named skip pointing at the ledger row that owns it, and the full
		// difference still rides on the row.
		row.Status = w8m5StatusSkip
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
func (h *w8m5EditHarness) file(row w8m5Row) {
	if len(h.unmeasurable) > 0 {
		if row.Status == w8m5StatusPass {
			row.Status = w8m5StatusSkip
		}
		row.Detail = strings.TrimSpace(row.Detail + " | NOT MEASURABLE: " + strings.Join(h.unmeasurable, " ;; "))
	}
	h.table.add(row)
}

// compareAgainstFreshIndex builds a fresh isolated index of the current tree
// and compares it with the incremental view, retrying while the two are still
// converging. It returns the surviving structural differences and how many
// probes differ once provenance is included.
func (h *w8m5EditHarness) compareAgainstFreshIndex(probes []w8m5Probe, drop map[string]bool) w8m5OracleResult {
	var residual []w8m5Difference
	var substantive int
	var empty []string
	structural := drop
	volatileOnly := w8m5VolatileKeys()
	// residualKeys additionally hides the measured semantic-metadata gap, so
	// the harness can say whether a difference is ONLY that gap or something
	// else as well.
	residualKeys := w8m5MergedKeys(structural, w8m5SemanticMetadataKeys())

	var differences []w8m5Difference
	provenance := 0
	for attempt := 1; attempt <= w8m5OracleAttempts; attempt++ {
		fresh := w8m5FreshIndex(h.t, h.binary, h.f.primary)
		incrementalStructural := w8m5Project(h.f, probes, structural)
		freshStructural := w8m5Project(fresh, probes, structural)
		differences = w8m5Compare(incrementalStructural, freshStructural, probes)
		substantive, empty = w8m5SubstantiveProbes(incrementalStructural, freshStructural, probes)
		provenance = len(w8m5Compare(w8m5Project(h.f, probes, volatileOnly), w8m5Project(fresh, probes, volatileOnly), probes))
		if len(differences) > 0 {
			residual = w8m5Compare(w8m5Project(h.f, probes, residualKeys), w8m5Project(fresh, probes, residualKeys), probes)
		} else {
			residual = nil
		}
		fresh.stop()
		if len(differences) == 0 {
			return w8m5OracleResult{Provenance: provenance, Substantive: substantive, Empty: empty}
		}
		if len(residual) == 0 {
			// The only difference is the characterized semantic-metadata
			// gap, which is sustained rather than converging: sampling
			// again would cost two more fresh indexes to learn nothing.
			return w8m5OracleResult{Differences: differences, Provenance: provenance, Substantive: substantive, Empty: empty}
		}
		if attempt < w8m5OracleAttempts {
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
	return w8m5OracleResult{Differences: differences, Residual: residual, Provenance: provenance,
		Substantive: substantive, Empty: empty}
}

func (h *w8m5EditHarness) fail(row *w8m5Row, detail string) {
	row.Status = w8m5StatusFail
	row.Detail = strings.TrimSpace(row.Detail + " | " + detail)
	h.t.Errorf("matrix2 %s (%s): %s", row.Case, row.Gate, detail)
}

// ------------------------------------------------------------ opt-in entry ---

// TestW8MatrixEditTaxonomy is matrix 2.
func TestW8MatrixEditTaxonomy(t *testing.T) {
	binary := w8m5Binary(t)
	spec := w8m5FixtureSpec(t)
	filter := w8m5CaseFilter(t)

	table := w8m5NewTable(t, "w8_matrix2_edits")
	defer table.render()

	f := w8m5NewFixture(t, binary, spec)
	defer f.stop()

	// Every case edits its own file: a difference a later case reports must
	// not be explainable by an earlier case's edit to the same file.
	rotation := w8RotationTargets(spec, len(w8m5EditCaseNames()))
	if len(rotation) < len(w8m5EditCaseNames()) {
		t.Fatalf("the corpus has %d rotation targets for %d cases; raise GXW8_MATRIX_FILES to at least %d",
			len(rotation), len(w8m5EditCaseNames()), len(w8m5EditCaseNames()))
	}
	h := &w8m5EditHarness{
		t: t, f: f, db: f.openReadOnly(), table: table, binary: binary, spec: spec,
		oracle:   os.Getenv("GXW8_MATRIX_ORACLE") != "0",
		backoff:  w8m5OracleBackoff(t),
		rotation: rotation,
		revision: map[int]int{},
	}
	for _, c := range w8m5EditCases() {
		if filter != nil && !filter.MatchString(c.Name) {
			table.add(w8m5Row{Case: c.Name, Gate: c.Gate, Status: w8m5StatusSkip,
				Detail: "not selected by GXW8_MATRIX_CASES"})
			continue
		}
		t.Logf("matrix2 case %s (%s)", c.Name, c.Gate)
		w8m5RunGuarded(t, table, c.Name, c.Gate, h.swapT, func() { h.run(c) })
	}
}

// ------------------------------------------------------------------ tests ---

func TestW8m5NormalizeMakesTwoDaemonsComparable(t *testing.T) {
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
	drop := w8m5MergedKeys(w8m5VolatileKeys(), w8m5ProvenanceKeys())
	if got, want := w8m5Canonical(w8m5Normalize(left, "/roots/left", drop)), w8m5Canonical(w8m5Normalize(right, "/roots/right", drop)); got != want {
		t.Fatalf("structural normalization did not make the two answers comparable:\n%s\n%s", got, want)
	}
	// With provenance kept, the same pair must differ: dropping it is a
	// deliberate, named exception, not an accident of the normalizer.
	volatile := w8m5VolatileKeys()
	if w8m5Canonical(w8m5Normalize(left, "/roots/left", volatile)) == w8m5Canonical(w8m5Normalize(right, "/roots/right", volatile)) {
		t.Fatal("provenance difference vanished even when provenance was kept")
	}
	// A real content difference must survive normalization.
	changed := map[string]any{
		"etag":  "cccc",
		"nodes": []any{map[string]any{"id": "issue767/p000/a.go::B", "absolute_file_path": "/roots/right/p000/a.go"}},
		"edges": []any{map[string]any{"from": "a", "to": "b", "kind": "reads", "line": 2.0, "origin": "ast_resolved"}},
	}
	if w8m5Canonical(w8m5Normalize(left, "/roots/left", drop)) == w8m5Canonical(w8m5Normalize(changed, "/roots/right", drop)) {
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
	if w8m5Canonical(w8m5Normalize(left, "/roots/left", drop)) == w8m5Canonical(w8m5Normalize(moved, "/roots/right", drop)) {
		t.Fatal("a moved location was normalized away")
	}
}

func TestW8m5CompareNamesTheProbeAndTheClause(t *testing.T) {
	probes := []w8m5Probe{
		{Key: "summary:p000/a.go", Clause: "locations"},
		{Key: "usages:issue767/p000/a.go::B", Clause: "incoming edges"},
		{Key: "symbols:B", Clause: "resolved target + deletion absence"},
	}
	same := map[string]string{"summary:p000/a.go": "{}", "usages:issue767/p000/a.go::B": "[]", "symbols:B": "{}"}
	if differences := w8m5Compare(same, same, probes); len(differences) != 0 {
		t.Fatalf("identical projections differed: %v", differences)
	}
	other := map[string]string{"summary:p000/a.go": "{}", "usages:issue767/p000/a.go::B": "[1]"}
	differences := w8m5Compare(same, other, probes)
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

func TestW8m5ProbesForCoverEveryGate1Clause(t *testing.T) {
	effect := w8m5Effect{
		Files:   []string{"p000/file00000.go"},
		Symbols: []w8m5Sighting{{"Fn00000S0", "p000/file00000.go"}},
		Present: []w8m5Sighting{{"W8Probe00000Rev1", "p000/file00000.go"}},
		Absent:  []w8m5Sighting{{"W8Probe00000Rev0", "p000/file00000.go"}},
		Observe: []w8m5Sighting{{"W8m5IgnoredSymbol", "w8m5_ignored/ignored.go"}},
		Texts:   []string{"w8Salt00000"},
	}
	probes := w8m5ProbesFor(effect)
	byKey := map[string]w8m5Probe{}
	for _, probe := range probes {
		byKey[probe.Key] = probe
	}
	for _, want := range []string{
		"summary:p000/file00000.go",
		"usages:issue767/p000/file00000.go::Fn00000S0",
		"source:issue767/p000/file00000.go::Fn00000S0",
		"symbols:W8Probe00000Rev1",
		"symbols:W8Probe00000Rev0",
		// An observed name is projected exactly like an asserted one: the
		// case does not judge it, the oracle still compares it.
		"symbols:W8m5IgnoredSymbol",
		"text:w8Salt00000",
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
	// daemon's root: w8m5Project substitutes the answering daemon's own root,
	// and a payload hard-wired to the other daemon's root would be answered
	// "file_not_indexed" — a difference the harness invented rather than
	// observed.
	summary := byKey["summary:p000/file00000.go"]
	path, _ := summary.Payload["path"].(string)
	if want := w8m5RootToken + string(filepath.Separator) + filepath.FromSlash("p000/file00000.go"); path != want {
		t.Fatalf("the summary probe path is %q, want the placeholder form %q", path, want)
	}
	for _, probe := range probes {
		rendered := w8m5Canonical(probe.Payload)
		if strings.Contains(rendered, "/private/") || strings.Contains(rendered, "gx767-") {
			t.Fatalf("probe %q carries a concrete daemon root: %s", probe.Key, rendered)
		}
	}
	// Asking the same question twice would double the cost and report one
	// difference twice.
	if doubled := w8m5ProbesFor(effect); len(doubled) != len(probes) {
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

func TestW8m5EditCasesCoverTheDeclaredTaxonomy(t *testing.T) {
	declared := w8m5EditCaseNames()
	cases := w8m5EditCases()
	if len(cases) != len(declared) {
		t.Fatalf("matrix 2 has %d cases, the declared taxonomy has %d", len(cases), len(declared))
	}
	for i, name := range declared {
		if cases[i].Name != name {
			t.Fatalf("case %d is %q, want %q (the handoff's own order)", i, cases[i].Name, name)
		}
		if cases[i].Apply == nil {
			t.Fatalf("case %q has no body", name)
		}
		if !strings.HasPrefix(cases[i].Gate, "gate") {
			t.Fatalf("case %q names no acceptance gate, got %q", name, cases[i].Gate)
		}
	}
	// The taxonomy's own classes, as the handoff spells them, each have to be
	// represented by at least one case.
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

func TestW8m5ReduceToNameKeepsTheSymbolAndDropsThePage(t *testing.T) {
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
	wanted := row("W8Probe00006Rev0", "p002/file00006.go", 45)

	// Two correct indexes whose ranked pages tie differently on unrelated
	// same-shaped names must compare equal: the ranking of symbols the case
	// never touched is not a property of the tree.
	left := page(wanted, row("W8Probe00013Rev0", "p001/file00013.go", 44), row("W8Probe00021Rev0", "p001/file00021.go", 44))
	right := page(row("W8Probe00000Rev0", "p000/file00000.go", 45), wanted, row("W8Probe00004Rev0", "p000/file00004.go", 45))
	if a, b := w8m5Canonical(w8m5ReduceToName(left, "W8Probe00006Rev0")), w8m5Canonical(w8m5ReduceToName(right, "W8Probe00006Rev0")); a != b {
		t.Fatalf("tie-break noise survived the reduction:\n%s\n%s", a, b)
	}

	// Deletion absence: a page that no longer carries the name reduces to
	// nothing, and that is different from a page that carries it.
	gone := page(row("W8Probe00013Rev0", "p001/file00013.go", 44))
	if got := w8m5Canonical(w8m5ReduceToName(gone, "W8Probe00006Rev0")); got != "[]" {
		t.Fatalf("a withdrawn symbol reduced to %s", got)
	}
	if w8m5Canonical(w8m5ReduceToName(gone, "W8Probe00006Rev0")) == w8m5Canonical(w8m5ReduceToName(left, "W8Probe00006Rev0")) {
		t.Fatal("present and absent reduced to the same answer; the probe would prove nothing")
	}

	// A real change to the symbol's own row survives: its file, its line and
	// its visibility are what the clause is about.
	for _, tc := range []struct {
		name string
		row  map[string]any
	}{
		{"moved to another file", row("W8Probe00006Rev0", "p002/other.go", 45)},
		{"moved line", row("W8Probe00006Rev0", "p002/file00006.go", 61)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := page(tc.row, row("W8Probe00013Rev0", "p001/file00013.go", 44))
			if w8m5Canonical(w8m5ReduceToName(changed, "W8Probe00006Rev0")) == w8m5Canonical(w8m5ReduceToName(left, "W8Probe00006Rev0")) {
				t.Fatalf("%s was reduced away", tc.name)
			}
		})
	}
	// Two rows for the same name (a duplicate the incremental view invented)
	// must not collapse into one.
	duplicated := page(wanted, wanted)
	if w8m5Canonical(w8m5ReduceToName(duplicated, "W8Probe00006Rev0")) == w8m5Canonical(w8m5ReduceToName(left, "W8Probe00006Rev0")) {
		t.Fatal("a duplicated row was reduced away")
	}
	// A row without an id is not a symbol row (a facet count, a suggestion).
	if got := w8m5Canonical(w8m5ReduceToName(map[string]any{"facets": []any{map[string]any{"name": "W8Probe00006Rev0", "count": 3.0}}}, "W8Probe00006Rev0")); got != "[]" {
		t.Fatalf("a non-symbol row was taken for a symbol: %s", got)
	}
}

func TestW8m5DirtyBuildSeriesIsABuildSeries(t *testing.T) {
	series := w8m5DirtyBuildSeries()
	if len(series) != 1 {
		t.Fatalf("the gate-4 reuse instrument is %d series: %v", len(series), series)
	}
	declared := map[string]bool{}
	for _, name := range viewmetrics.SeriesNames() {
		declared[name] = true
	}
	if !declared[w8m5SeriesName(series[0])] {
		t.Fatalf("series %q is not declared in the viewmetrics catalog; the reuse claim would read a permanent zero", series[0])
	}
	// It has to be a series that only a BUILD moves. A reuse claim phrased
	// against a series that also moves on reuse — or on merely serving a
	// request — would pass no matter what the daemon did.
	allocation := map[string]bool{}
	for _, key := range w8m5AllocationSeries() {
		allocation[key] = true
	}
	if !allocation[series[0]] {
		t.Fatalf("series %q is not in the allocation family, so 'it did not move' is not evidence of reuse", series[0])
	}
	for _, key := range w8m5ReplaySeries() {
		if key == series[0] {
			t.Fatalf("series %q is the replay family's own series; reuse would look like a rebuild", key)
		}
	}
}

func TestW8m5SymbolIDIsTheProductSpelling(t *testing.T) {
	if got, want := w8m5SymbolID("p000/file00000.go", "Fn00000S0"), "issue767/p000/file00000.go::Fn00000S0"; got != want {
		t.Fatalf("symbol id %q, want %q", got, want)
	}
}

func TestW8m5CopyTreeReproducesTheServedBytes(t *testing.T) {
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
	if err := os.Symlink(filepath.Join("p000", "a.go"), filepath.Join(from, "link.go")); err != nil {
		t.Skipf("this host refuses symlinks: %v", err)
	}
	w8m5CopyTree(t, from, to)

	if source, err := os.ReadFile(filepath.Join(to, "p000", "a.go")); err != nil || string(source) != "package p000\n" {
		t.Fatalf("copied content: %q %v", source, err)
	}
	info, err := os.Stat(filepath.Join(to, "p000", "x.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the executable bit was lost: %v", info.Mode())
	}
	if _, err := os.Lstat(filepath.Join(to, "link.go")); err != nil {
		t.Fatalf("the symlink was not reproduced: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(to, "link.go")); err != nil || target != filepath.Join("p000", "a.go") {
		t.Fatalf("symlink target %q %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(to, ".git")); !os.IsNotExist(err) {
		t.Fatalf("the Git directory was copied into the oracle fixture: %v", err)
	}
}

func TestW8m5DescribeDifferenceNamesTheEntryThatDiffers(t *testing.T) {
	left := `{"edges":[{"from":"A","kind":"calls","line":8,"to":"B"},{"from":"A","kind":"reads","line":2,"to":"C"}],"total_edges":2}`
	right := `{"edges":[{"from":"A","kind":"calls","line":8,"to":"B"},{"from":"A","kind":"reads","line":2,"to":"C"},{"from":"A","kind":"value_flow","line":8,"to":"D"}],"total_edges":3}`
	described := w8m5DescribeDifference(left, right)
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
	if described := w8m5DescribeDifference(right, left); !strings.Contains(described, "only in incremental") {
		t.Fatalf("the incremental-only side was not named: %s", described)
	}
	// A missing key, not a differing one.
	if described := w8m5DescribeDifference(`{"a":1}`, `{"a":1,"b":2}`); !strings.Contains(described, ".b only in fresh") {
		t.Fatalf("a key present on one side only was not named: %s", described)
	}
	// Equal answers describe nothing, and a non-JSON answer still renders.
	if described := w8m5DescribeDifference(left, left); !strings.Contains(described, "compare equal") {
		t.Fatalf("equal answers described as %q", described)
	}
	if described := w8m5DescribeDifference("call error: boom", `{"a":1}`); !strings.Contains(described, "call error: boom") {
		t.Fatalf("a non-JSON answer was swallowed: %s", described)
	}
}

func TestW8m5ListMinusIsAMultisetDifference(t *testing.T) {
	left := []any{"a", "b", "b"}
	right := []any{"b", "c"}
	if got := w8m5ListMinus(left, right); len(got) != 2 || got[0] != `"a"` || got[1] != `"b"` {
		t.Fatalf("multiset difference %v", got)
	}
	if got := w8m5ListMinus(right, left); len(got) != 1 || got[0] != `"c"` {
		t.Fatalf("reverse multiset difference %v", got)
	}
	if got := w8m5ListMinus(left, left); len(got) != 0 {
		t.Fatalf("a list differed from itself: %v", got)
	}
}

func TestW8m5SemanticMetadataGapIsNamedAndNarrow(t *testing.T) {
	// The gap hides exactly three keys and nothing else: widening it would
	// blind the gate-1 oracle to real differences.
	gap := w8m5SemanticMetadataKeys()
	if len(gap) != 3 || !gap["semantic_source"] || !gap["semantic_type"] || !gap["return_type"] {
		t.Fatalf("the known-gap key set drifted: %v", gap)
	}
	for key := range w8m5MergedKeys(w8m5VolatileKeys(), w8m5ProvenanceKeys()) {
		if gap[key] {
			t.Fatalf("key %q is in two exclusion families; a reader cannot tell which rule hid it", key)
		}
	}
	if strings.TrimSpace(w8m5KnownGapReason) == "" || !strings.Contains(w8m5KnownGapReason, "W6.11") {
		t.Fatalf("the skip reason must name the ledger row that owns the gap: %q", w8m5KnownGapReason)
	}

	// A node that differs ONLY in those keys is explained by the gap; a node
	// that also lost an edge, a location or a signature is not.
	incremental := map[string]any{"nodes": []any{map[string]any{"id": "A", "start_line": 6.0,
		"meta": map[string]any{"signature": "func A() int"}}}}
	fresh := map[string]any{"nodes": []any{map[string]any{"id": "A", "start_line": 6.0,
		"meta": map[string]any{"signature": "func A() int", "semantic_source": "go-types", "semantic_type": "func() int", "return_type": "(int)"}}}}
	structural := w8m5MergedKeys(w8m5VolatileKeys(), w8m5ProvenanceKeys())
	residual := w8m5MergedKeys(structural, gap)
	if w8m5Canonical(w8m5Normalize(incremental, "", structural)) == w8m5Canonical(w8m5Normalize(fresh, "", structural)) {
		t.Fatal("the semantic-metadata difference was already invisible to the structural comparison")
	}
	if w8m5Canonical(w8m5Normalize(incremental, "", residual)) != w8m5Canonical(w8m5Normalize(fresh, "", residual)) {
		t.Fatal("the residual comparison did not attribute the difference to the known gap")
	}
	moved := map[string]any{"nodes": []any{map[string]any{"id": "A", "start_line": 7.0,
		"meta": map[string]any{"signature": "func A() int", "semantic_source": "go-types", "semantic_type": "func() int", "return_type": "(int)"}}}}
	if w8m5Canonical(w8m5Normalize(incremental, "", residual)) == w8m5Canonical(w8m5Normalize(moved, "", residual)) {
		t.Fatal("a moved location was excused by the known-gap exclusion")
	}
}

// TestW8m5ReuseVerdictRefusesADeadInstrument pins the gate-4 decision: a
// "did not move" reading is reuse only when a step that really did rebuild
// moved the same series. The review found the shipped reading unfalsifiable —
// no dirty build in either matrix ever moved a coordinator_cycle series, so
// "built_dirty did not move" carried no information while the row claimed gate 4
// confirmed.
func TestW8m5ReuseVerdictRefusesADeadInstrument(t *testing.T) {
	series := w8m5DirtyBuildSeries()
	key := series[0]

	t.Run("dead instrument is not measurable", func(t *testing.T) {
		text, measurable, reused := w8m5ReuseVerdict("undo", map[string]int64{}, map[string]int64{}, series)
		if measurable || reused {
			t.Fatalf("a silent calibration reported measurable=%v reused=%v: %s", measurable, reused, text)
		}
		if !strings.Contains(text, "NOT MEASURABLE") || !strings.Contains(text, key) {
			t.Fatalf("the verdict did not say why it could not be scored: %s", text)
		}
	})

	t.Run("live instrument that did not move is reuse", func(t *testing.T) {
		text, measurable, reused := w8m5ReuseVerdict("undo", map[string]int64{key: 1}, map[string]int64{}, series)
		if !measurable || !reused {
			t.Fatalf("a live calibration with a still window reported measurable=%v reused=%v: %s", measurable, reused, text)
		}
		if !strings.Contains(text, "reused") {
			t.Fatalf("the verdict did not report reuse: %s", text)
		}
	})

	t.Run("live instrument that moved is a rebuild", func(t *testing.T) {
		text, measurable, reused := w8m5ReuseVerdict("redo", map[string]int64{key: 1}, map[string]int64{key: 1}, series)
		if !measurable || reused {
			t.Fatalf("a moved window reported measurable=%v reused=%v: %s", measurable, reused, text)
		}
		if !strings.Contains(text, "REBUILT") {
			t.Fatalf("the verdict did not report a rebuild: %s", text)
		}
	})

	t.Run("a calibration that moved an unrelated series is still dead", func(t *testing.T) {
		unrelated := map[string]int64{w8m5Series(viewmetrics.FamilyInventorySeconds): 3}
		if _, measurable, _ := w8m5ReuseVerdict("undo", unrelated, map[string]int64{}, series); measurable {
			t.Fatal("movement outside the named series was accepted as calibration")
		}
	})
}

// TestW8m5TrivialAnswerKnowsAnEmptyOrRefusedAnswer pins what does not count as
// evidence: an empty reduction and a transport failure recorded as the answer.
func TestW8m5TrivialAnswerKnowsAnEmptyOrRefusedAnswer(t *testing.T) {
	for _, value := range []string{"", "  ", "[]", "{}", "null",
		"call error: exit status 1: file_not_indexed", "decode error: unexpected end of JSON input",
		"marshal error: unsupported type"} {
		if !w8m5TrivialAnswer(value) {
			t.Fatalf("%q was counted as evidence", value)
		}
	}
	for _, value := range []string{`[{"id":"a"}]`, `{"nodes":[]}`, `{"summary":{"kind":"function"}}`} {
		if w8m5TrivialAnswer(value) {
			t.Fatalf("%q was discarded as carrying no evidence", value)
		}
	}
}

// TestW8m5SubstantiveProbesCountsOnlyRealAgreement is the F8 rule: two empty
// answers, or two error strings, are not a probe that agreed.
func TestW8m5SubstantiveProbesCountsOnlyRealAgreement(t *testing.T) {
	probes := []w8m5Probe{
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
	substantive, empty := w8m5SubstantiveProbes(incremental, fresh, probes)
	if substantive != 1 {
		t.Fatalf("substantive = %d, want 1 (only the summary)", substantive)
	}
	if len(empty) != 2 || empty[0] != "source:broken" || empty[1] != "symbols:Gone" {
		t.Fatalf("probes carrying no evidence = %v, want [source:broken symbols:Gone]", empty)
	}
}

// TestW8m5GateOneVerdictKeepsAResidualDifferenceAFailure pins matrix 2's own
// decision. The review's mutation — turning the residual arm into a skip — must
// turn this red.
func TestW8m5GateOneVerdictKeepsAResidualDifferenceAFailure(t *testing.T) {
	gap := []w8m5Difference{{Probe: "summary:p000/file00000.go", Clause: "metadata",
		Incremental: `{"meta":{}}`, Fresh: `{"meta":{"semantic_source":"go-types"}}`}}
	residual := []w8m5Difference{{Probe: "usages:x", Clause: "incoming edges",
		Incremental: `{"edges":[]}`, Fresh: `{"edges":[{"from":"y"}]}`}}

	t.Run("no projection", func(t *testing.T) {
		got := w8m5GateOneVerdict(w8m5OracleInput{Probes: 0})
		if got.Status != w8m5StatusFail || !strings.Contains(strings.Join(got.Failures, " "), "no projection") {
			t.Fatalf("verdict = %+v, want a FAIL naming the missing projection", got)
		}
	})

	t.Run("nothing substantive", func(t *testing.T) {
		got := w8m5GateOneVerdict(w8m5OracleInput{Probes: 2, Substantive: 0, Empty: []string{"symbols:Gone", "source:broken"}})
		if got.Status != w8m5StatusFail {
			t.Fatalf("a comparison of two empty answers = %s, want FAIL: %+v", got.Status, got)
		}
		if !strings.Contains(strings.Join(got.Failures, " "), "agree for free") {
			t.Fatalf("the failure did not say why: %+v", got)
		}
	})

	t.Run("equal", func(t *testing.T) {
		got := w8m5GateOneVerdict(w8m5OracleInput{Probes: 4, Substantive: 3, Empty: []string{"symbols:Gone"}})
		if got.Status != w8m5StatusPass {
			t.Fatalf("verdict = %+v, want PASS", got)
		}
		if !strings.Contains(strings.Join(got.Recorded, " "), "3 substantive") {
			t.Fatalf("the PASS did not report how much of it was substantive: %+v", got)
		}
	})

	t.Run("only the named gap is a named skip", func(t *testing.T) {
		got := w8m5GateOneVerdict(w8m5OracleInput{Probes: 4, Substantive: 3, Differences: gap})
		if got.Status != w8m5StatusSkip {
			t.Fatalf("verdict = %+v, want SKIP", got)
		}
		joined := strings.Join(got.Recorded, " ")
		if !strings.Contains(joined, "W6.1b") {
			t.Fatalf("the skip did not name the ledger row that owns the gap: %s", joined)
		}
		for _, wrong := range []string{"ledger rows W3.2 + W3.7"} {
			if strings.Contains(joined, wrong) {
				t.Fatalf("the skip still cites a row that does not own the gap (%q): %s", wrong, joined)
			}
		}
	})

	t.Run("a residual difference is a failure", func(t *testing.T) {
		got := w8m5GateOneVerdict(w8m5OracleInput{Probes: 4, Substantive: 3,
			Differences: append(append([]w8m5Difference{}, gap...), residual...),
			Residual:    residual, Attempts: w8m5OracleAttempts, Backoff: time.Minute})
		if got.Status != w8m5StatusFail {
			t.Fatalf("a difference beyond the named gap = %s, want FAIL: %+v", got.Status, got)
		}
		joined := strings.Join(got.Failures, " ")
		if !strings.Contains(joined, "usages:x") || !strings.Contains(joined, "beyond the known semantic-metadata gap") {
			t.Fatalf("the failure did not name the residual probe: %s", joined)
		}
	})
}

// TestW8m5KnownGapReasonNamesTheRowThatOwnsIt pins the attribution the review
// found contradicted by the ledger: W3.2 and W6.11 are `wired` and do not own
// this gap; W6.1b's declared limitation (2) does.
func TestW8m5KnownGapReasonNamesTheRowThatOwnsIt(t *testing.T) {
	if !strings.Contains(w8m5KnownGapReason, "W6.1b") {
		t.Fatalf("the skip reason does not name the owning ledger row: %s", w8m5KnownGapReason)
	}
	if !strings.Contains(w8m5KnownGapReason, "not the closure") {
		t.Fatalf("the skip reason does not quote the limitation it rests on: %s", w8m5KnownGapReason)
	}
	if !strings.Contains(w8m5KnownGapReason, "do NOT own this") {
		t.Fatalf("the skip reason does not retract the rows that do not own the gap: %s", w8m5KnownGapReason)
	}
}

// TestW8m5EditCasesDeclareAComparableProjection pins that every case projects
// something a fresh index can be compared against — the precondition
// w8m5GateOneVerdict refuses at run time.
func TestW8m5EditCasesDeclareAComparableProjection(t *testing.T) {
	// An absence-only effect — the shape file_deleted had when the review
	// found it resting on two empty row lists — can only produce probes that
	// reduce to nothing on both sides. w8m5GateOneVerdict refuses such a
	// comparison at run time (TestW8m5GateOneVerdictKeepsAResidualDifferenceAFailure
	// /nothing_substantive); this pins that the projection really is that thin,
	// so the refusal is the only thing standing between it and a free PASS.
	deleted := w8m5Effect{Absent: []w8m5Sighting{{"Gone", "p002/gone.go"}}}
	if probes := w8m5ProbesFor(deleted); len(probes) != 1 {
		t.Fatalf("an absence-only effect produced %d probes, want 1", len(probes))
	}
	withControl := w8m5Effect{
		Files:   []string{"p000/file00000.go"},
		Symbols: []w8m5Sighting{{"Fn00000S0", "p000/file00000.go"}},
		Absent:  []w8m5Sighting{{"Gone", "p002/gone.go"}},
	}
	if probes := w8m5ProbesFor(withControl); len(probes) < 4 {
		t.Fatalf("an effect with a control produced %d probes, want at least 4", len(probes))
	}
}

// TestW8m5FileDowngradesAnUnmeasurableClaim pins that a case which asked for a
// claim this instrument cannot carry never files a PASS: the row becomes a
// named SKIP saying which claim could not be scored.
func TestW8m5FileDowngradesAnUnmeasurableClaim(t *testing.T) {
	table := w8m5NewTable(t, "w8m5_file_rule")
	h := &w8m5EditHarness{t: t, table: table}

	h.unmeasurable = nil
	h.file(w8m5Row{Case: "measurable", Gate: "gate4", Status: w8m5StatusPass, Detail: "reused"})

	h.unmeasurable = []string{"undo: NOT MEASURABLE — a step that really did rebuild moved none of [views_coordinator_cycle_total{outcome=built_dirty}]"}
	h.file(w8m5Row{Case: "unmeasurable", Gate: "gate4", Status: w8m5StatusPass, Detail: "reused"})
	h.file(w8m5Row{Case: "already failing", Gate: "gate4", Status: w8m5StatusFail, Detail: "gate1: differs"})

	if len(table.rows) != 3 {
		t.Fatalf("filed %d rows, want 3", len(table.rows))
	}
	if table.rows[0].Status != w8m5StatusPass {
		t.Fatalf("a measurable PASS was downgraded: %+v", table.rows[0])
	}
	if table.rows[1].Status != w8m5StatusSkip {
		t.Fatalf("an unmeasurable claim still filed %s: %+v", table.rows[1].Status, table.rows[1])
	}
	if !strings.Contains(table.rows[1].Detail, "NOT MEASURABLE") {
		t.Fatalf("the skip did not say which claim could not be scored: %+v", table.rows[1])
	}
	if table.rows[2].Status != w8m5StatusFail {
		t.Fatalf("an unmeasurable claim upgraded a FAIL to %s: %+v", table.rows[2].Status, table.rows[2])
	}
	if problems := w8m5TableProblems(table.rows); len(problems) != 0 {
		t.Fatalf("the filed rows are not reportable: %v", problems)
	}
}
