package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// E2E matrix 3: resolution, provenance and manifests.
//
// One opt-in isolated-daemon run over the shared sustained-workload fixture
// (issue767_fixture_shared_test.go), plus the offline unit tests for this
// file's own pure logic, which run in every ordinary `go test` of this package.
//
// The matrix bullet this item owns:
//
//	real imported consumers versus unrelated same-named modules;
//	nested/vendor/invalid/oversized/unreadable manifests; missing metadata;
//	dynamic-language and cross-repository producer controls; exact
//	restub/provenance behaviour.
//
// It is hazard H8 — "module-root/directory-mismatch placement,
// manifest-only invalidation, mixed-language imports and provider mutation
// evidence remain distinct from the implemented positive Go ownership
// rejection gate" — which gets one case each here.
//
// Three rules this file obeys:
//
//  1. Every assertion names the acceptance gate it serves, and the oracle is a
//     FRESH ISOLATED INDEX of the same tree — never a node count. `nodes` and
//     `edges` row counts appear nowhere in this file's assertions.
//  2. A case this host cannot exercise is recorded as `not_exercised` with a
//     reason naming the ledger row. A case the branch does not handle is
//     recorded as `documented_gap` against a declared entry in
//     e2eResolutionKnownGaps. Neither is a silent pass: e2eResolutionValidateOutcomes fails
//     the run if a declared case produces no outcome row, and the classifier
//     fails the run if a divergence matches no declared gap.
//  3. A declared gap that no longer reproduces is a FAILURE, not a quiet win:
//     the entry has to be deleted and the ledger row closed.

const (
	// e2eResolutionBinaryEnv opts into the isolated-daemon matrix. The value is a
	// path to a gortex binary built from the tree under test.
	e2eResolutionBinaryEnv = "GX_E2E_MATRIX_BINARY"
	// e2eResolutionArtifactEnv is where the outcome table and the raw projections are
	// written. Without it they go to the test's own temporary directory and
	// vanish with the run; the table is always also written to the test log.
	e2eResolutionArtifactEnv = "GX_E2E_MATRIX_ARTIFACT_DIR"
	// e2eResolutionLedgerRow is the execution-ledger row every skip reason in this
	// file names, so a skip can always be traced back to a tracked item.
	e2eResolutionLedgerRow = "e2e matrix 3: resolution, provenance and manifests"
	// e2eResolutionSkipReason is the skip text for the whole file's opt-in gate.
	e2eResolutionSkipReason = "set " + e2eResolutionBinaryEnv + " to opt into the isolated resolution/provenance/manifest matrix (ledger row " + e2eResolutionLedgerRow + ")"
)

// e2eResolutionMaxFileSize is the index.max_file_size this matrix configures, and
// e2eResolutionOversizedBytes is the size a file must exceed it by. The cap defaults
// to zero (no cap, internal/config/config.go:665), so the oversized arm only
// exists because this run configures one — which is why the run states the
// knob rather than pretending the default skips anything.
//
// The cap is a SOURCE-file gate, not a manifest gate, and that is measured
// rather than assumed:
//
//   - internal/indexer/walk_source.go:83-91 — admitUnexcludedWalkFile asks
//     effectiveLanguage first and returns walkAdmission{} for a path no
//     extractor claims; the MaxFileSize comparison at :88 is never reached for
//     a manifest.
//   - internal/indexer/walk_source.go:99-112 — the scoped walk's manifest
//     escape hatch passes size -1 and so never meets the cap either.
//   - internal/indexer/incremental_contracts.go:103-109 — only a
//     ROOT-relative "go.mod"/"go.work" is a manifest at all, so a nested
//     go.mod is neither a manifest nor a source file.
//   - internal/indexer/indexer.go:9450-9469 — the census computes
//     `oversize := supported && …`, so an unsupported path is never oversize.
//   - internal/indexer/size_skip_census.go:48 says it outright: "Language-less
//     root manifests are not subject to that cap."
//
// So the oversized arm asserts on an oversized SOURCE file, where the cap
// really runs and mints a size-skip stub
// (internal/indexer/skip_telemetry.go:199-215, emitted at indexer.go:4219),
// and the oversized-manifest case is declared unexercisable up front.
const (
	e2eResolutionMaxFileSize    = 1 << 20
	e2eResolutionOversizedBytes = 2 << 20
)

// The second repository. It declares the SAME module path and the same package
// and symbol names as the primary, so "cross-repository producer controls"
// means something: the primary's consumer must keep binding to its own file.
const (
	e2eResolutionOtherRepoDir  = "other"
	e2eResolutionOtherRepoName = "otherrepo"
)

// Node identities under test. They are repository-prefixed, slash-relative and
// therefore identical in two fixtures over the same tree — which is what makes
// a fresh isolated index usable as an oracle at all.
const (
	e2eResolutionConsumeID      = issue767FixturePrefix + "/consumer/consumer.go::Consume"
	e2eResolutionProduceID      = issue767FixturePrefix + "/producer/producer.go::Produce"
	e2eResolutionDecoyProduceID = issue767FixturePrefix + "/decoy/producer/producer.go::Produce"
	e2eResolutionDecoyOnlyID    = issue767FixturePrefix + "/decoy/producer/producer.go::GxResolutionDecoyOnly"
	e2eResolutionWithdrawnID    = issue767FixturePrefix + "/producer/producer.go::Withdrawn"
	e2eResolutionMarkerID       = issue767FixturePrefix + "/marker.go::Issue767PrimaryMarker"
	e2eResolutionRubyID         = issue767FixturePrefix + "/nometa/nometa.rb::missing_metadata_value"
	e2eResolutionProducerFile   = issue767FixturePrefix + "/producer/producer.go"
	e2eResolutionGoModRel       = "go.mod"

	// The oversized-SOURCE arm. big.go is over the configured cap and small.go
	// is the control beside it: if the cap did not run, big.go's symbol would
	// be in the graph exactly as small.go's is.
	e2eResolutionBigSourceFileID  = issue767FixturePrefix + "/bigsource/big.go"
	e2eResolutionBigSourceSymID   = issue767FixturePrefix + "/bigsource/big.go::GxResolutionOversizedValue"
	e2eResolutionBigControlFileID = issue767FixturePrefix + "/bigsource/small.go"
	e2eResolutionBigControlSymID  = issue767FixturePrefix + "/bigsource/small.go::GxResolutionOversizedControlValue"
	e2eResolutionBigSourceSymName = "GxResolutionOversizedValue"

	// The sibling beside the unparseable manifest. Without it "an unparseable
	// manifest does not stop its siblings being indexed" has no sibling.
	e2eResolutionBrokenSiblingID = issue767FixturePrefix + "/broken/broken.go::GxResolutionBrokenSiblingValue"

	// The dynamic pair: one entry point reached only through
	// importlib.import_module (unprovable) and one static sibling that resolves
	// the same target through an ordinary import (provable).
	e2eResolutionDynDynamicID = issue767FixturePrefix + "/dyn/app.py::run"
	e2eResolutionDynStaticID  = issue767FixturePrefix + "/dyn/app.py::call_helper"

	// The mixed-language triple. app.ts imports helper.js by name; the decoy
	// exports the SAME name from a TypeScript file nothing imports. The case
	// asserts the named cross-language bind EXISTS and that the same-named
	// symbol in the other language is NOT what it bound to — an assertion that
	// fails three separate ways, unlike a filter over rows the query already
	// constrained.
	e2eResolutionMixedEntryID    = issue767FixturePrefix + "/mixed/app.ts::mixedEntry"
	e2eResolutionMixedHelperID   = issue767FixturePrefix + "/mixed/helper.js::mixedHelperValue"
	e2eResolutionMixedDecoyID    = issue767FixturePrefix + "/mixeddecoy/helper.ts::mixedHelperValue"
	e2eResolutionMixedDecoyPfx   = issue767FixturePrefix + "/mixeddecoy/"
	e2eResolutionMixedNamedBind  = e2eResolutionMixedEntryID + " -calls-> " + e2eResolutionMixedHelperID
	e2eResolutionMixedHelperName = "mixedHelperValue"
)

// ---------------------------------------------------------------------------
// The corpus.
// ---------------------------------------------------------------------------

// e2eResolutionSourceFile is one file in the matrix corpus. Unreadable files are
// committed readable (git has to read them) and chmod-ed to 0 afterwards.
type e2eResolutionSourceFile struct {
	Path       string
	Content    string
	Unreadable bool
}

// e2eResolutionProducerRevision names the four producer/producer.go revisions the
// provenance stages walk through. The line numbers are part of the contract:
// they are how a re-parse is observed landing without inventing a new symbol.
type e2eResolutionProducerRevision int

const (
	// e2eResolutionProducerCold is the first commit: Produce declared on line 3.
	e2eResolutionProducerCold e2eResolutionProducerRevision = iota
	// e2eResolutionProducerCommented is the idempotent re-parse: two comment lines
	// only, so the symbol identity is unchanged and Produce moves to line 5.
	e2eResolutionProducerCommented
	// e2eResolutionProducerWithdrawn deletes Produce and leaves a different symbol, so
	// the file still exists and the change is a deletion, not a file removal.
	e2eResolutionProducerWithdrawn
	// e2eResolutionProducerRestored is byte-identical to e2eResolutionProducerCommented.
	e2eResolutionProducerRestored
)

// e2eResolutionProduceLine is the line Produce is declared on in a revision, or 0 when
// the revision does not declare it.
func e2eResolutionProduceLine(rev e2eResolutionProducerRevision) int64 {
	switch rev {
	case e2eResolutionProducerCold:
		return 3
	case e2eResolutionProducerCommented, e2eResolutionProducerRestored:
		return 5
	}
	return 0
}

func e2eResolutionProducerSource(rev e2eResolutionProducerRevision) string {
	switch rev {
	case e2eResolutionProducerCold:
		return "package producer\n\nfunc Produce() int { return 7 }\n"
	case e2eResolutionProducerCommented, e2eResolutionProducerRestored:
		return "package producer\n\n// Produce returns the produced value.\n// The comment is the whole edit: the declared symbol is unchanged.\nfunc Produce() int { return 7 }\n"
	case e2eResolutionProducerWithdrawn:
		return "package producer\n\nfunc Withdrawn() int { return 7 }\n"
	}
	return ""
}

func e2eResolutionDecoySource(edited bool) string {
	if edited {
		return "package producer\n\nfunc Produce() int { return 1234 }\n\nfunc GxResolutionDecoyOnly() int { return 5 }\n"
	}
	return "package producer\n\nfunc Produce() int { return 99 }\n"
}

func e2eResolutionRootManifest(edited bool) string {
	base := "module example.invalid/issue767\n\ngo 1.24\n"
	if edited {
		return base + "\n// manifest-only edit: not one source file changes with it.\n"
	}
	return base
}

// e2eResolutionCorpusFiles is the primary repository's matrix corpus, in a fixed order.
// Every manifest shape the item names has a file here, and every case in
// e2eResolutionCases names the files it needs so the two cannot drift apart
// (TestE2EMatrixResolutionEveryCaseHasItsCorpus).
func e2eResolutionCorpusFiles() []e2eResolutionSourceFile {
	files := []e2eResolutionSourceFile{
		// The WORKSPACE config. index.max_file_size has to live here and
		// nowhere else: ConfigManager.GetRepoConfig
		// (internal/config/manager.go:281-286) returns the repository's own
		// .gortex.yaml when it has one and compiled Default() when it does
		// not — the user-level config.yaml's `index:` block never reaches a
		// repository's indexer, so a cap written there is silently absent and
		// every oversized arm would observe an uncapped index while reporting
		// a cap. internal/config/config.go:660-665 says the same in prose
		// ("via `.gortex.yaml`").
		{Path: ".gortex.yaml", Content: e2eResolutionWorkspaceConfig()},
		// The real module and its one real imported consumer.
		{Path: "go.mod", Content: e2eResolutionRootManifest(false)},
		{Path: "marker.go", Content: "package fixture\n\n// Issue767PrimaryMarker is the fixture's readiness probe.\nfunc Issue767PrimaryMarker() int { return 1 }\n"},
		{Path: "producer/producer.go", Content: e2eResolutionProducerSource(e2eResolutionProducerCold)},
		{Path: "consumer/consumer.go", Content: "package consumer\n\nimport \"example.invalid/issue767/producer\"\n\nfunc Consume() int { return producer.Produce() }\n"},
		// An unrelated module with the same package name and the same symbol
		// name, in a NESTED manifest whose module root is not the directory
		// the importer resolves from (H8 module-root mismatch).
		{Path: "decoy/go.mod", Content: "module example.invalid/decoy\n\ngo 1.24\n"},
		{Path: "decoy/producer/producer.go", Content: e2eResolutionDecoySource(false)},
		// A vendor manifest.
		{Path: "vendor/modules.txt", Content: "# example.invalid/vendored v1.0.0\n## explicit; go 1.24\nexample.invalid/vendored\n"},
		{Path: "vendor/example.invalid/vendored/vendored.go", Content: "package vendored\n\nfunc Produce() int { return -7 }\n"},
		// An invalid manifest: not parseable as a module file at all, with a
		// perfectly ordinary source file beside it. The sibling is the case:
		// an unparseable manifest must not take its directory down with it.
		{Path: "broken/go.mod", Content: "this is not a module file {{{\n\x00\x01not go.mod syntax\n"},
		{Path: "broken/broken.go", Content: "package broken\n\n// GxResolutionBrokenSiblingValue sits beside an unparseable manifest.\nfunc GxResolutionBrokenSiblingValue() int { return 17 }\n"},
		// An oversized manifest. Kept in the corpus because the matrix bullet
		// names the shape, but its case is declared UNEXERCISABLE: the cap is
		// never consulted on a manifest (see e2eResolutionMaxFileSize's citations), so
		// no observation of it could come out differently at any size.
		{Path: "oversized/go.mod", Content: e2eResolutionOversizedManifest()},
		// The oversized arm that can actually fail: a valid Go file over the
		// cap, and a small one beside it in the same package. The cap runs on
		// both (both are claimed by an extractor); only the big one is dropped,
		// and it is dropped into a visible size-skip stub rather than silence.
		{Path: "bigsource/big.go", Content: e2eResolutionOversizedSource()},
		{Path: "bigsource/small.go", Content: "package bigsource\n\n// GxResolutionOversizedControlValue is the in-spec control beside the oversized file.\nfunc GxResolutionOversizedControlValue() int { return 19 }\n"},
		// An unreadable manifest.
		{Path: "unreadable/go.mod", Content: "module example.invalid/unreadable\n\ngo 1.24\n", Unreadable: true},
		// An unreadable SOURCE file beside the unreadable manifest. A manifest
		// below the repository root is not in the corpus at all (measured: a
		// nested go.mod earns no node), so the manifest arm on its own could
		// only ever observe silence; the source file is the one the pass
		// really tries to read.
		{Path: "unreadable/unreadable.go", Content: "package unreadable\n\nfunc UnreadableValue() int { return 11 }\n", Unreadable: true},
		// Missing metadata: a language file with no manifest of its own kind
		// anywhere in the tree.
		{Path: "nometa/nometa.rb", Content: "def missing_metadata_value\n  41 + 1\nend\n"},
		// A dynamic language: one dynamic import that can bind to anything and
		// one static sibling import that cannot.
		{Path: "dyn/__init__.py", Content: ""},
		{Path: "dyn/helper.py", Content: "def helper_value():\n    return 3\n"},
		{Path: "dyn/app.py", Content: "import importlib\n\nfrom . import helper\n\n\ndef run(name):\n    module = importlib.import_module(name)\n    return module.helper_value()\n\n\ndef call_helper():\n    return helper.helper_value()\n"},
		// Mixed-language imports: TypeScript importing JavaScript.
		{Path: "mixed/helper.js", Content: "export function mixedHelperValue() {\n  return 9;\n}\n"},
		{Path: "mixed/app.ts", Content: "import { mixedHelperValue } from \"./helper.js\";\n\nexport function mixedEntry(): number {\n  return mixedHelperValue();\n}\n"},
		// The decoy for the mixed-language case: the SAME exported name, in
		// the OTHER language of the pair, in a directory nothing imports.
		// Without it, "the TypeScript entry point did not bind across the
		// language boundary by name" has nothing it could have bound to
		// wrongly, and the case can only confirm the query's own WHERE clause.
		{Path: "mixeddecoy/helper.ts", Content: "export function mixedHelperValue(): number {\n  return -9;\n}\n"},
	}
	return files
}

// e2eResolutionWorkspaceConfig is the primary repository's .gortex.yaml — the only
// file from which index.max_file_size actually reaches the indexer.
func e2eResolutionWorkspaceConfig() string {
	return "index:\n  max_file_size: " + strconv.Itoa(e2eResolutionMaxFileSize) + "\n"
}

// e2eResolutionOversizedManifest is a syntactically valid module file padded past
// e2eResolutionMaxFileSize with comment lines.
func e2eResolutionOversizedManifest() string {
	var b strings.Builder
	b.WriteString("module example.invalid/oversized\n\ngo 1.24\n")
	line := "// " + strings.Repeat("x", 96) + "\n"
	for b.Len() < e2eResolutionOversizedBytes {
		b.WriteString(line)
	}
	return b.String()
}

// e2eResolutionOversizedSource is a syntactically valid Go file padded past
// e2eResolutionMaxFileSize. The declaration comes FIRST so that an indexer which
// ignored the cap and parsed the file would certainly find the symbol — the
// assertion's failure mode has to be reachable, not merely plausible.
func e2eResolutionOversizedSource() string {
	var b strings.Builder
	b.WriteString("package bigsource\n\n// GxResolutionOversizedValue is declared before the padding on purpose.\nfunc GxResolutionOversizedValue() int { return 23 }\n")
	line := "// " + strings.Repeat("x", 96) + "\n"
	for b.Len() < e2eResolutionOversizedBytes {
		b.WriteString(line)
	}
	return b.String()
}

// e2eResolutionCorpusFileSize is a declared corpus file's size in bytes, or -1 when the
// corpus does not declare it. Sizes are what the oversized arms report, and
// reporting a size the corpus does not actually carry would be worse than
// reporting none.
func e2eResolutionCorpusFileSize(path string) int {
	for _, file := range e2eResolutionCorpusFiles() {
		if file.Path == path {
			return len(file.Content)
		}
	}
	return -1
}

// e2eResolutionOtherRepoFiles is the second tracked repository: the same module path,
// the same package, the same symbol. Nothing in the primary imports it.
func e2eResolutionOtherRepoFiles() []e2eResolutionSourceFile {
	return []e2eResolutionSourceFile{
		{Path: "go.mod", Content: "module example.invalid/issue767\n\ngo 1.24\n"},
		{Path: "producer/producer.go", Content: "package producer\n\nfunc Produce() int { return -1 }\n"},
		{Path: "consumer/consumer.go", Content: "package consumer\n\nimport \"example.invalid/issue767/producer\"\n\nfunc Consume() int { return producer.Produce() }\n"},
	}
}

// ---------------------------------------------------------------------------
// Cases and outcomes.
// ---------------------------------------------------------------------------

type e2eResolutionCase struct {
	// ID is the outcome table's key and the name a ledger row cites.
	ID string
	// Gate is the acceptance gate this case serves, in the matrix's numbering.
	Gate string
	// Hazard is the resolution hazard this case covers, when it covers one.
	Hazard string
	// Needs are the corpus paths the case reads. They are checked against
	// e2eResolutionCorpusFiles offline so a corpus edit cannot orphan a case.
	Needs []string
	// What the case asserts, in one line, for the outcome table.
	What string
	// Falsifier names the OBSERVABLE that would make this case fail on this
	// corpus: the concrete row that would have to appear, or disappear, for
	// the assertion to come out false. It is written down per case because the
	// failure this file keeps producing is an assertion whose query already
	// guarantees its own predicate — a pass the run did not earn. If no such
	// observable exists on this corpus, the case has no Falsifier and carries
	// an Unexercisable declaration instead; the two are mutually exclusive and
	// exactly one of them is required (TestE2EMatrixResolutionEveryCaseSaysHowItCouldFail).
	//
	// A Falsifier is not decoration: for every case below, the named row is
	// either produced by this corpus today (so the assertion is a real
	// comparison) or its absence is itself what the case reports.
	Falsifier string
	// Unexercisable declares UP FRONT, in source, that this branch cannot
	// exercise the case, and says why. It exists because an assertion that
	// cannot fail is worse than no assertion: it hands the Suite stage a pass
	// the run did not produce. When it is set:
	//
	//   - e2eResolutionValidateOutcomes REQUIRES the runtime row to be
	//     not_exercised. A pass recorded for such a case fails the run.
	//   - the offline corpus test exempts it from the rule that a case naming
	//     a manifest must also name a file that manifest's directory holds —
	//     the very rule whose violation makes a manifest case vacuous.
	Unexercisable string
}

const (
	e2eResolutionPass         = "pass"
	e2eResolutionFail         = "FAIL"
	e2eResolutionGap          = "documented_gap"
	e2eResolutionNotExercised = "not_exercised"
	e2eResolutionObserved     = "observed"
)

// e2eResolutionStatuses is every status an outcome row may carry.
func e2eResolutionStatuses() []string {
	return []string{e2eResolutionPass, e2eResolutionFail, e2eResolutionGap, e2eResolutionNotExercised, e2eResolutionObserved}
}

// e2eResolutionCases is the declared matrix. Every one of these produces exactly one
// outcome row, or the run fails.
func e2eResolutionCases() []e2eResolutionCase {
	return []e2eResolutionCase{
		{ID: "real_imported_consumer_binds_its_own_module", Gate: "G1", Needs: []string{"consumer/consumer.go", "producer/producer.go"},
			What:      "the one real imported consumer resolves to the producer its manifest names",
			Falsifier: "the consumer→producer calls edge absent from the served generation — the row stage 4 of this very matrix observes disappearing when the definition is withdrawn"},
		{ID: "unrelated_same_named_module_is_not_bound", Gate: "G1", Needs: []string{"decoy/go.mod", "decoy/producer/producer.go", "consumer/consumer.go"},
			What:      "no edge binds the consumer to the same-named symbol of a module nothing imports, and that module IS in the graph so the absence is a resolution fact rather than an empty directory",
			Falsifier: "an edge from issue767/consumer/% to issue767/decoy/producer/producer.go::Produce — the exact row the withdraw stage observes this branch creating — or the decoy's own Produce node missing, which would make the absence vacuous"},
		{ID: "nested_manifest_module_root_mismatch", Gate: "G1", Hazard: "H8 module-root mismatch", Needs: []string{"decoy/go.mod", "decoy/producer/producer.go"},
			What:      "a nested manifest's directory is not treated as the importer's module root, and its own source file is indexed all the same",
			Falsifier: "any edge from outside issue767/decoy/ landing inside it, or the nested module's own source file missing from the served generation"},
		{ID: "vendor_manifest", Gate: "G1", Needs: []string{"vendor/modules.txt", "vendor/example.invalid/vendored/vendored.go", "decoy/producer/producer.go"},
			What:      "the real consumer did not bind into the vendored copy of the same-named symbol — asserted only while the vendored tree contributes nodes an edge could target, and recorded not_exercised with that measurement when it does not",
			Falsifier: "an edge from outside issue767/vendor/ into it (the same-named vendored Produce capturing the consumer), which is observable only while the vendored tree is in the graph; when it contributes no node at all there is nothing an edge could name and the case is recorded not_exercised rather than passed"},
		{ID: "invalid_manifest", Gate: "G1", Needs: []string{"broken/go.mod", "broken/broken.go"},
			What:      "an unparseable manifest does not stop the source file in its own directory being indexed",
			Falsifier: "issue767/broken/broken.go::GxResolutionBrokenSiblingValue missing from the served generation — the unparseable manifest taking its own directory down with it"},
		{ID: "oversized_manifest", Gate: "G8", Needs: []string{"oversized/go.mod"},
			What: "a manifest over index.max_file_size is skipped, and the pass still completes",
			Unexercisable: "index.max_file_size is never consulted on a manifest at any size: " +
				"admitUnexcludedWalkFile drops a path no extractor claims before the cap (internal/indexer/walk_source.go:83-91), " +
				"the scoped walk's manifest escape hatch passes no size at all (internal/indexer/walk_source.go:99-112), " +
				"only a ROOT-relative go.mod/go.work is a manifest in the first place (internal/indexer/incremental_contracts.go:103-109), " +
				"the census computes oversize only for a supported language (internal/indexer/indexer.go:9450-9469), " +
				"and internal/indexer/size_skip_census.go:48 states that language-less root manifests are not subject to the cap — " +
				"so every observation of this corpus path is identical at 1 KiB and at 2 MiB and the assertion could not fail"},
		{ID: "oversized_source_is_size_skipped", Gate: "G8", Needs: []string{"bigsource/big.go", "bigsource/small.go", ".gortex.yaml"},
			What:      "a SOURCE file over index.max_file_size earns a file node carrying the size-skip telemetry instead of its symbols, while the in-spec file beside it is indexed normally and carries no such telemetry",
			Falsifier: "GxResolutionOversizedValue present in the served generation (observed in run 7, when the cap sat in a config file GetRepoConfig never reads), or big.go's node missing the skipped_due_to_size marker, or the in-spec control's node carrying it — the last of which would mean the marker probe reports the same thing for every file and discriminates nothing"},
		{ID: "unreadable_manifest", Gate: "G9", Needs: []string{"unreadable/go.mod", "unreadable/unreadable.go"},
			What:      "an unreadable path leaves recorded evidence in the served generation, not silence",
			Falsifier: "no file_index_failures row for the unreadable path at view_gen 0 under the primary's own repo_prefix AND no node from it — the silence this case exists to refuse"},
		{ID: "missing_metadata", Gate: "G1", Needs: []string{"nometa/nometa.rb"},
			What:      "a language file with no manifest of its own kind is still extracted",
			Falsifier: "no node at all from nometa/nometa.rb, the shape a metadata-gated extractor would produce"},
		{ID: "dynamic_language_conservative", Gate: "G3", Hazard: "H8 dynamic placement", Needs: []string{"dyn/app.py", "dyn/helper.py"},
			What:      "a bind reached only through importlib.import_module is distinguishable from the static sibling's bind to the same target — the same standard this matrix applies to origin=text_matched everywhere else",
			Falsifier: "a bind out of run() onto a concrete repository symbol carrying the same kind, the same target and the same provenance as call_helper()'s proof of that target (observed on the candidate: this case FAILS), or the static sibling producing no bind at all, which would leave nothing to compare a guess against"},
		{ID: "mixed_language_imports", Gate: "G1", Hazard: "H8 mixed-language imports", Needs: []string{"mixed/app.ts", "mixed/helper.js", "mixeddecoy/helper.ts"},
			What:      "the TypeScript entry point binds across the language boundary to the JavaScript file it names, and not to the same-named symbol of the other language in a directory it does not import",
			Falsifier: "the named bind issue767/mixed/app.ts::mixedEntry -calls-> issue767/mixed/helper.js::mixedHelperValue missing from the served generation; or any edge out of issue767/mixed/ landing on issue767/mixeddecoy/helper.ts::mixedHelperValue (a by-name bind that crossed into the wrong language's file); or that decoy symbol missing from the graph, which would make the not-bound half vacuous"},
		{ID: "unrelated_module_edit_does_not_disturb_real_binding", Gate: "G3", Needs: []string{"decoy/producer/producer.go"},
			What:      "editing the unrelated same-named module leaves the real binding byte-identical",
			Falsifier: "the consumer's full provenance row differing before and after an edit to a file it does not import, or vanishing"},
		{ID: "manifest_only_invalidation", Gate: "G2", Hazard: "H8 manifest-only invalidation", Needs: []string{"go.mod"},
			What:      "a manifest-only edit keeps every binding and is measured for over-invalidation",
			Falsifier: "the consumer's full provenance row differing after an edit that changes no source file, or vanishing"},
		{ID: "restub_identity_and_provenance_preserved", Gate: "G3", Needs: []string{"producer/producer.go", "consumer/consumer.go"},
			What:      "an idempotent re-parse keeps the incoming edge's identity AND loses no provenance-bearing row around the re-parsed file — the provenance half is checked on rows that actually carry provenance, not on the calls edge whose columns are empty",
			Falsifier: "a provenance-bearing row dropped by the comment-only re-parse measured alone, or the incoming edge changing across it (observed on the candidate: two value_flow rows go, and this case FAILS)"},
		{ID: "declared_gaps_are_caused_by_the_reparse_alone", Gate: "G3", Needs: []string{"producer/producer.go", "consumer/consumer.go"},
			What:      "every declared known gap blames the comment-only re-parse, and the re-parse alone — measured before the withdraw/restore sequence — really is what drops its rows",
			Falsifier: "a declared gap whose row the isolated re-parse measurement did not drop in the projection and direction the gap names"},
		{ID: "unresolved_facts_degrade_truthfully", Gate: "G1", Needs: []string{"producer/producer.go"},
			What:      "a deleted definition leaves no edge claiming a resolved tier and no false rebind",
			Falsifier: "a surviving call edge whose unresolved terminal still advertises a tier, or one rebound onto the same-named symbol in decoy/, vendor/ or the second repository (observed on the candidate: the decoy rebind, this case FAILS)"},
		{ID: "rebind_restores_exact_identity", Gate: "G4", Needs: []string{"producer/producer.go"},
			What:      "restoring the definition restores the exact incoming identity and provenance",
			Falsifier: "the incoming edge after a byte-identical restore differing from the row recorded before the withdrawal, or being absent (observed on the candidate: absent, this case FAILS)"},
		{ID: "cross_repository_producer_control", Gate: "G1", Needs: []string{"consumer/consumer.go"},
			What:      "a second repository with the same module path and symbol changes no binding",
			Falsifier: "a resolver-bound reference out of the primary landing in otherrepo/ — the deflection a second repository declaring the same module path could cause — or the primary's own binding missing; when the second repository produces no node at all the case is recorded not_exercised with that measurement"},
		{ID: "provider_mutation_evidence", Gate: "G6", Hazard: "H8 provider mutation evidence", Needs: []string{"producer/producer.go"},
			What:      "an enrichment provider's recorded state is evidence of the mutation it saw",
			Falsifier: "a provider row whose recorded coverage contradicts the mutation sequence it observed; this isolated build runs no provider at all, so the case is recorded not_exercised with the rows that were present rather than passed"},
		{ID: "context_separated_build_matches_fresh_index", Gate: "G1+G3", Needs: []string{"producer/producer.go", "consumer/consumer.go"},
			What:      "the incrementally reached view equals a fresh isolated index over the ten derived outputs",
			Falsifier: "any row of a compared projection present on one arm and absent on the other that no declared gap explains, or a declared gap that stopped reproducing (observed on the candidate: four unmatched rows, this case FAILS)"},
	}
}

// e2eResolutionOutcome is one row of the run's outcome table.
type e2eResolutionOutcome struct {
	Case   string `json:"case"`
	Gate   string `json:"gate"`
	Hazard string `json:"hazard,omitempty"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// e2eResolutionRecorder collects outcomes and fails the test for a real failure. It is
// the only place a case may be marked, and e2eResolutionValidateOutcomes checks after
// the run that every declared case was marked exactly once.
type e2eResolutionRecorder struct {
	t       *testing.T
	cases   map[string]e2eResolutionCase
	rows    []e2eResolutionOutcome
	perCase map[string]int
}

func e2eResolutionNewRecorder(t *testing.T) *e2eResolutionRecorder {
	cases := map[string]e2eResolutionCase{}
	for _, c := range e2eResolutionCases() {
		cases[c.ID] = c
	}
	return &e2eResolutionRecorder{t: t, cases: cases, perCase: map[string]int{}}
}

func (r *e2eResolutionRecorder) record(id, status, format string, args ...any) {
	c, ok := r.cases[id]
	if !ok {
		r.t.Fatalf("resolution matrix: outcome recorded for undeclared case %q", id)
	}
	r.perCase[id]++
	r.rows = append(r.rows, e2eResolutionOutcome{Case: id, Gate: c.Gate, Hazard: c.Hazard, Status: status, Detail: fmt.Sprintf(format, args...)})
	if status == e2eResolutionFail {
		r.t.Errorf("resolution matrix %s (%s): %s", id, c.Gate, fmt.Sprintf(format, args...))
	}
}

// assert is the ordinary shape: a condition that has to hold, with the detail
// recorded either way so the outcome table shows what was actually observed.
func (r *e2eResolutionRecorder) assert(id string, ok bool, format string, args ...any) {
	status := e2eResolutionFail
	if ok {
		status = e2eResolutionPass
	}
	r.record(id, status, format, args...)
}

// observe records a measurement that is evidence rather than a contract.
func (r *e2eResolutionRecorder) observe(id, format string, args ...any) {
	r.record(id, e2eResolutionObserved, format, args...)
}

// e2eResolutionNothingAsserted is the phrase every runtime skip carries. A case that
// declares a Falsifier says what would make it fail; if the run cannot produce
// that observable the row has to say, in the outcome table, that the case
// asserted nothing — a not_exercised row is evidence of a gap in the run, never
// of a property of the branch.
const e2eResolutionNothingAsserted = "nothing was asserted"

// skip records a case this host or this build cannot exercise. The reason must
// name the ledger row; e2eResolutionValidateOutcomes enforces it.
func (r *e2eResolutionRecorder) skip(id, reason string) {
	r.record(id, e2eResolutionNotExercised, "%s — %s (ledger row %s)", reason, e2eResolutionNothingAsserted, e2eResolutionLedgerRow)
}

// skipMeasured records a case whose falsifying observable this corpus did not
// produce in this run, together with the measurement that shows it. It is the
// alternative to asserting a predicate the corpus already guarantees: if there
// is no row that could have made the case fail, the case did not pass.
func (r *e2eResolutionRecorder) skipMeasured(id, control string) {
	c, ok := r.cases[id]
	if !ok {
		r.t.Fatalf("resolution matrix: outcome recorded for undeclared case %q", id)
	}
	if c.Falsifier == "" {
		r.t.Fatalf("resolution matrix: case %q declares no falsifying observable; it is unexercisable by declaration, not by measurement", id)
	}
	r.record(id, e2eResolutionNotExercised,
		"the observable that could have made this case fail is absent from this run, so %s; the case needs: %s; measured this run: %s (ledger row %s)",
		e2eResolutionNothingAsserted, c.Falsifier, control, e2eResolutionLedgerRow)
}

// skipDeclared records a case whose Unexercisable reason is declared in source,
// together with the control this run actually measured. The declared reason is
// reproduced verbatim so e2eResolutionValidateOutcomes can check the row against the
// declaration, and the control is what proves the run looked rather than
// assumed.
func (r *e2eResolutionRecorder) skipDeclared(id, control string) {
	c, ok := r.cases[id]
	if !ok {
		r.t.Fatalf("resolution matrix: outcome recorded for undeclared case %q", id)
	}
	if c.Unexercisable == "" {
		r.t.Fatalf("resolution matrix: case %q is not declared unexercisable; use skip() with a measured reason", id)
	}
	r.record(id, e2eResolutionNotExercised, "%s; measured control this run: %s (ledger row %s)", c.Unexercisable, control, e2eResolutionLedgerRow)
}

// e2eResolutionValidateOutcomes is the "never a silent pass" rule as code: every
// declared case has exactly one row, every status is a declared one, and every
// not_exercised row names the ledger row.
func e2eResolutionValidateOutcomes(cases []e2eResolutionCase, rows []e2eResolutionOutcome) []string {
	valid := map[string]bool{}
	for _, status := range e2eResolutionStatuses() {
		valid[status] = true
	}
	seen := map[string]int{}
	var problems []string
	for _, row := range rows {
		seen[row.Case]++
		if !valid[row.Status] {
			problems = append(problems, fmt.Sprintf("case %s: unknown status %q", row.Case, row.Status))
		}
		if row.Status == e2eResolutionNotExercised && !strings.Contains(row.Detail, e2eResolutionLedgerRow) {
			problems = append(problems, fmt.Sprintf("case %s: not_exercised without a ledger row in its reason", row.Case))
		}
		if strings.TrimSpace(row.Detail) == "" {
			problems = append(problems, fmt.Sprintf("case %s: empty detail", row.Case))
		}
	}
	byCase := map[string]e2eResolutionOutcome{}
	for _, row := range rows {
		if _, already := byCase[row.Case]; !already {
			byCase[row.Case] = row
		}
	}
	declared := map[string]bool{}
	for _, c := range cases {
		declared[c.ID] = true
		switch seen[c.ID] {
		case 1:
		case 0:
			problems = append(problems, fmt.Sprintf("case %s: declared but never recorded — a silent pass", c.ID))
		default:
			problems = append(problems, fmt.Sprintf("case %s: recorded %d times", c.ID, seen[c.ID]))
		}
		// Every case says either how it could fail or why it cannot be
		// exercised, and never both: that is the rule this round exists to
		// enforce, and it is checked at run time as well as offline so a case
		// added later cannot slip past with neither.
		switch {
		case c.Falsifier == "" && c.Unexercisable == "":
			problems = append(problems, fmt.Sprintf("case %s: declares neither a falsifying observable nor an unexercisable reason — an assertion nobody can say how to break is not a result", c.ID))
		case c.Falsifier != "" && c.Unexercisable != "":
			problems = append(problems, fmt.Sprintf("case %s: declares both a falsifying observable and an unexercisable reason; it is one or the other", c.ID))
		}
		// A case declared unexercisable in source may only ever be recorded
		// not_exercised. Anything else — above all a pass — would be an
		// assertion that cannot fail dressed up as a result.
		if c.Unexercisable == "" {
			// A case that CAN fail, skipped at run time, has to say in the row
			// that it asserted nothing — otherwise a reader of the table sees a
			// non-failure where the run simply never looked.
			if row, ok := byCase[c.ID]; ok && row.Status == e2eResolutionNotExercised && !strings.Contains(row.Detail, e2eResolutionNothingAsserted) {
				problems = append(problems, fmt.Sprintf("case %s: not_exercised without saying that %s — record it through skip/skipMeasured so the table cannot read as a quiet non-failure", c.ID, e2eResolutionNothingAsserted))
			}
			continue
		}
		row, ok := byCase[c.ID]
		if !ok {
			continue
		}
		if row.Status != e2eResolutionNotExercised {
			problems = append(problems, fmt.Sprintf("case %s: declared unexercisable in source but recorded %q — an assertion that cannot fail must not be reported as a result", c.ID, row.Status))
		}
		if !strings.Contains(row.Detail, c.Unexercisable) {
			problems = append(problems, fmt.Sprintf("case %s: not_exercised without the declared reason from its case", c.ID))
		}
	}
	for id := range seen {
		if !declared[id] {
			problems = append(problems, fmt.Sprintf("case %s: recorded but not declared", id))
		}
	}
	sort.Strings(problems)
	return problems
}

// e2eResolutionRenderOutcomes renders the outcome table this item has to hand back.
func e2eResolutionRenderOutcomes(rows []e2eResolutionOutcome) string {
	var b strings.Builder
	b.WriteString("Outcome table (E2E matrix 3 — resolution / provenance / manifests)\n")
	b.WriteString(strings.Repeat("-", 78) + "\n")
	for _, row := range rows {
		hazard := ""
		if row.Hazard != "" {
			hazard = " [" + row.Hazard + "]"
		}
		fmt.Fprintf(&b, "%-14s %-6s %s%s\n                 %s\n", row.Status, row.Gate, row.Case, hazard, row.Detail)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The ten derived outputs, as projections of a store.
// ---------------------------------------------------------------------------

// e2eResolutionOutput is one projection of a store, compared arm-to-arm. Derived marks
// the ten outputs the enrichment census names as the ones a naive reuse drops;
// the rest are the
// gate-1 support projections (identities, locations, inventory, declarations)
// without which "the ten survived" would say nothing.
type e2eResolutionOutput struct {
	ID      string
	Derived bool
	// RecordOnly marks a projection that is written to both artifact files but
	// excluded from the comparison, because the two arms legitimately reach it
	// by different histories. It exists so that narrowing a compared projection
	// to the served generation does not delete the rows it stops comparing:
	// they move here instead of disappearing. A RecordOnly projection may never
	// be one of the ten derived outputs — that is checked offline.
	RecordOnly bool
	Query      string
	// Why the projection is the observable for that output.
	Why string
}

// e2eResolutionDerivedOutputIDs is the enrichment census's list, in its order. An offline test pins it
// so an output cannot quietly leave the matrix.
func e2eResolutionDerivedOutputIDs() []string {
	return []string{
		"restub_provenance",
		"incoming_edges_from_context_files",
		"ref_fact_sidecar_rows",
		"unresolved_fact_degradation",
		"pathless_resolver_stubs",
		"edge_source_markers",
		"clone_similarity_symmetry",
		"symbol_content_fts",
		"lsp_enrichment_claims",
		"capability_dataflow_framework_edges",
	}
}

// e2eResolutionResolvedEdgeKinds are the resolver-bound reference kinds; they are the
// "incoming edges from context files" observable.
const e2eResolutionResolvedEdgeKinds = "'calls','imports','implements','extends','inherits','references','instantiates','uses','method_call'"

// e2eResolutionDerivedEdgeKinds are the capability / dataflow / framework-synth kinds.
const e2eResolutionDerivedEdgeKinds = "'value_flow','dataflow','reads_env','executes_process','accesses_field','emits','handles','dispatches','routes','synthesized'"

// e2eResolutionStructuralEdgeKinds are the extractor's own containment edges.
const e2eResolutionStructuralEdgeKinds = "'defines','contains','declares'"

// The oracle compares the PRIMARY repository's view. The second repository is
// deliberately ambiguous — it declares the same module path as the primary — so
// leaving its own rows in the comparison would report that ambiguity as a
// reuse defect. Its facts are asserted and recorded by
// e2eResolutionStageCrossRepository instead, in both directions.
//
// An edge is the primary's when the primary owns its source symbol; a node is
// the primary's by repo_prefix. The value_flow edge from a primary symbol into
// the second repository's consumer is therefore still compared: it is a fact
// the primary's generation produced.
const (
	e2eResolutionPrimaryEdgeScope = " AND from_id LIKE '" + issue767FixturePrefix + "%'"
	e2eResolutionPrimaryRepoScope = " AND repo_prefix='" + issue767FixturePrefix + "'"
)

// e2eResolutionOutputs is every projection the oracle compares. Each query returns one
// text column; rows are sorted and compared as sets of strings, so a diff reads
// as "this exact fact was present in the fresh index and absent here".
//
// Everything is scoped to view_gen = 0. That is the generation the primary
// actually serves from on this branch: the primary's own working route stays on
// legacy generation 0 until the working-route promotion lands, which this
// branch defers. The probe run confirmed it: a dirty edit lands in view_gen 0
// while the published committed base stays frozen at the commit.
func e2eResolutionOutputs() []e2eResolutionOutput {
	return []e2eResolutionOutput{
		{ID: "restub_provenance", Derived: true,
			Why: "provenance rides on the edge's own origin/tier/confidence columns; a restub clears them and a same-target rebind restores them",
			Query: "SELECT from_id||' -'||kind||'-> '||to_id||' @'||file_path||':'||line||" +
				"' origin='||origin||' tier='||tier||' conf='||CAST(confidence AS TEXT)||' label='||confidence_label " +
				"FROM edges WHERE view_gen=0" + e2eResolutionPrimaryEdgeScope + " AND (origin<>'' OR tier<>'' OR confidence<>0 OR confidence_label<>'')"},
		{ID: "incoming_edges_from_context_files", Derived: true,
			Why: "a resolver-bound reference edge whose target lives in a file the pass only read as context",
			Query: "SELECT from_id||' -'||kind||'-> '||to_id||' @'||file_path||':'||line " +
				"FROM edges WHERE view_gen=0" + e2eResolutionPrimaryEdgeScope + " AND kind IN (" + e2eResolutionResolvedEdgeKinds + ")"},
		{ID: "ref_fact_sidecar_rows", Derived: true,
			Why: "the resolved-reference sidecar, replaced set-wise per (repo_prefix, file)",
			Query: "SELECT from_id||' -'||kind||'-> '||to_id||' ref='||ref_name||' line='||CAST(line AS TEXT)||" +
				"' origin='||origin||' tier='||tier||' cand='||CAST(candidates AS TEXT)||' @'||file_path||' lang='||lang " +
				"FROM ref_facts WHERE view_gen=0" + e2eResolutionPrimaryRepoScope},
		{ID: "unresolved_fact_degradation", Derived: true,
			Why: "an unresolved or external terminal must not advertise a tier it no longer earns",
			Query: "SELECT from_id||' -'||kind||'-> '||to_id||' origin='||origin||' tier='||tier||' conf='||CAST(confidence AS TEXT) " +
				"FROM edges WHERE view_gen=0" + e2eResolutionPrimaryEdgeScope + " AND (to_id LIKE 'unresolved::%' OR to_id LIKE '%::unresolved::%' OR to_id LIKE 'external::%' OR to_id LIKE '%::external::%')"},
		{ID: "pathless_resolver_stubs", Derived: true,
			Why:   "builtin and external stubs carry no file path and so belong to no file's change set",
			Query: "SELECT id||' kind='||kind||' name='||name||' lang='||language FROM nodes WHERE view_gen=0" + e2eResolutionPrimaryRepoScope + " AND file_path=''"},
		{ID: "edge_source_markers", Derived: true,
			Why: "generation_edge_sources is what hides a contested edge source; an absent marker is a silent claim. " +
				"Scoped to the SERVED generation like every other projection: the arm reached its view through five edits and the oracle through one cold index, so their non-served generations differ by construction and comparing them could never pass. Those rows are recorded, uncompared, by nonserved_generation_markers.",
			Query: "SELECT DISTINCT 'gen0 '||source_id||' '||ownership_mode FROM generation_edge_sources WHERE view_gen=0"},
		{ID: "clone_similarity_symmetry", Derived: true,
			Why: "near-duplicate detection ranks a body against a corpus, so a sparse generation's corpus is not the repository's",
			Query: "SELECT DISTINCT 'shingle '||node_id||' tokens='||CAST(token_count AS TEXT) FROM clone_shingles WHERE view_gen=0" + e2eResolutionPrimaryRepoScope + " " +
				"UNION SELECT DISTINCT 'corpus '||repo_prefix||' gen0' FROM clone_corpus_state WHERE view_gen=0 AND repo_prefix='" + issue767FixturePrefix + "'"},
		{ID: "symbol_content_fts", Derived: true,
			Why: "the two search sidecars; they are inherited through the composed stack and have to be shown so. " +
				"symbol_fts / content_fts are FTS5 virtual tables with no generation column (internal/graph/store_sqlite/schema.go:1052,1066), so each row is joined to its OWNERSHIP sidecar — symbol_fts_rowid / content_fts_rowid, both keyed on view_gen (schema.go:1251-1269) — and only the rows the served generation owns are compared. Without the join the projection mixes in rows a later generation wrote, which is a difference in history rather than in the view.",
			Query: "SELECT DISTINCT 'symbol '||s.node_id FROM symbol_fts s JOIN symbol_fts_rowid r ON r.fts_rowid=s.rowid " +
				"WHERE r.view_gen=0 AND r.repo_prefix='" + issue767FixturePrefix + "' " +
				"UNION SELECT DISTINCT 'content '||c.node_id||':'||CAST(c.ordinal AS TEXT) FROM content_fts c JOIN content_fts_rowid cr ON cr.fts_rowid=c.rowid " +
				"WHERE cr.view_gen=0 AND cr.repo_prefix='" + issue767FixturePrefix + "'"},
		{ID: "lsp_enrichment_claims", Derived: true,
			Why: "a generation that could not run language-server enrichment has to declare it rather than report an absence as a fact",
			Query: "SELECT DISTINCT 'gen0 '||producer||' '||state||' '||reason " +
				"FROM generation_producer_completeness WHERE view_gen=0 AND producer LIKE 'lsp.%'"},
		{ID: "capability_dataflow_framework_edges", Derived: true,
			Why: "capability, dataflow and framework-synthesised edges are produced after resolution and are not re-derived by a reused payload",
			Query: "SELECT from_id||' -'||kind||'-> '||to_id||' @'||file_path||':'||line||' origin='||origin " +
				"FROM edges WHERE view_gen=0" + e2eResolutionPrimaryEdgeScope + " AND kind IN (" + e2eResolutionDerivedEdgeKinds + ")"},

		// Support projections. Not part of the ten, but gate 1 asks for
		// identities, locations, visibility, ownership and declarations too.
		{ID: "nodes_and_locations",
			Why: "gate 1's identities, locations, visibility and signatures",
			Query: "SELECT id||' kind='||kind||' name='||name||' qual='||qual_name||' @'||file_path||':'||CAST(start_line AS TEXT)||'-'||CAST(end_line AS TEXT)||" +
				"' lang='||language||' vis='||COALESCE(visibility,'')||' sig='||COALESCE(signature,'') FROM nodes WHERE view_gen=0" + e2eResolutionPrimaryRepoScope},
		{ID: "structural_edges",
			Why: "the extractor's own containment edges, so a lost definition cannot hide in an unqueried kind",
			Query: "SELECT from_id||' -'||kind||'-> '||to_id||' @'||file_path||':'||CAST(line AS TEXT) " +
				"FROM edges WHERE view_gen=0" + e2eResolutionPrimaryEdgeScope + " AND kind IN (" + e2eResolutionStructuralEdgeKinds + ")"},
		{ID: "other_edges",
			Why: "the catch-all: any edge kind none of the other projections claim, so no kind escapes the oracle",
			Query: "SELECT from_id||' -'||kind||'-> '||to_id||' @'||file_path||':'||CAST(line AS TEXT) FROM edges WHERE view_gen=0" + e2eResolutionPrimaryEdgeScope +
				" AND kind NOT IN (" + e2eResolutionResolvedEdgeKinds + ") AND kind NOT IN (" + e2eResolutionDerivedEdgeKinds + ") AND kind NOT IN (" + e2eResolutionStructuralEdgeKinds + ")"},
		{ID: "files_inventory",
			Why:   "which files the generation claims, and the extraction errors it recorded for them",
			Query: "SELECT file_path||' nodes='||CAST(node_count AS TEXT)||' errors='||errors FROM files WHERE view_gen=0" + e2eResolutionPrimaryRepoScope},
		{ID: "producer_completeness",
			Why:   "every producer's completeness declaration, not only the language-server lanes",
			Query: "SELECT DISTINCT 'gen0 '||producer||' '||state||' '||reason FROM generation_producer_completeness WHERE view_gen=0"},
		{ID: "index_failures",
			Why:   "a file the pass could not read has to be a recorded failure, not an absence",
			Query: "SELECT file_path||' err='||error||' denied='||CAST(permission_denied AS TEXT) FROM file_index_failures WHERE view_gen=0 AND repo_prefix='" + issue767FixturePrefix + "'"},

		// Recorded, NOT compared. Two arms over the same tree reach the served
		// generation by different histories — five edits against one cold
		// index — so their NON-served generations differ by construction and no
		// fresh index could ever reproduce them. Comparing them would make the
		// oracle unpassable; dropping them silently would hide what the arms
		// carry. This projection is the third option: written to both artifact
		// files every run, asserted on by nothing.
		{ID: "nonserved_generation_markers", RecordOnly: true,
			Why: "the generation-keyed rows OUTSIDE the served view, recorded uncompared and asserted on by nothing, so scoping the compared projections to view_gen 0 hides nothing. " +
				"It carries one arm for EVERY generation-keyed table a compared projection reads — pinned by TestE2EMatrixResolutionNonServedMarkersCoverEveryComparedGenerationTable — so a projection added to the compared set cannot narrow the observation without the rows it stops comparing turning up here.",
			Query: e2eResolutionNonServedQuery()},
	}
}

// e2eResolutionNonServedArm records the rows OUTSIDE the served generation for one
// generation-keyed table that a compared projection reads.
type e2eResolutionNonServedArm struct {
	// Table is the generation-keyed table this arm records.
	Table string
	// Select is the arm's query. It must scope itself to view_gen<>0: this
	// projection is the complement of the compared ones, not a duplicate.
	Select string
}

// e2eResolutionFTSVirtualTables are the two FTS5 virtual tables. They carry no
// generation column at all (internal/graph/store_sqlite/schema.go:1052,1066),
// so there is no "non-served row" of theirs to record; their ownership
// sidecars — symbol_fts_rowid / content_fts_rowid, both keyed on view_gen
// (schema.go:1251-1269) — are the generation-keyed tables, and both have arms.
func e2eResolutionFTSVirtualTables() []string { return []string{"symbol_fts", "content_fts"} }

// e2eResolutionNonServedArms is one arm per generation-keyed table any COMPARED
// projection reads. The coverage is checked offline rather than trusted: the
// claim "scoping the comparison to the served generation hides nothing" is only
// as wide as this list.
func e2eResolutionNonServedArms() []e2eResolutionNonServedArm {
	const repo = issue767FixturePrefix
	return []e2eResolutionNonServedArm{
		{Table: "nodes", Select: "SELECT 'node gen'||CAST(view_gen AS TEXT)||' '||id||' kind='||kind FROM nodes WHERE view_gen<>0 AND repo_prefix='" + repo + "'"},
		{Table: "edges", Select: "SELECT 'edge gen'||CAST(view_gen AS TEXT)||' '||from_id||' -'||kind||'-> '||to_id FROM edges WHERE view_gen<>0 AND from_id LIKE '" + repo + "%'"},
		{Table: "ref_facts", Select: "SELECT 'ref_fact gen'||CAST(view_gen AS TEXT)||' '||from_id||' -'||kind||'-> '||to_id||' ref='||ref_name FROM ref_facts WHERE view_gen<>0 AND repo_prefix='" + repo + "'"},
		{Table: "files", Select: "SELECT 'file gen'||CAST(view_gen AS TEXT)||' '||file_path||' nodes='||CAST(node_count AS TEXT) FROM files WHERE view_gen<>0 AND repo_prefix='" + repo + "'"},
		{Table: "file_index_failures", Select: "SELECT 'index_failure gen'||CAST(view_gen AS TEXT)||' '||file_path||' denied='||CAST(permission_denied AS TEXT) FROM file_index_failures WHERE view_gen<>0 AND repo_prefix='" + repo + "'"},
		{Table: "clone_shingles", Select: "SELECT DISTINCT 'shingle gen'||CAST(view_gen AS TEXT)||' '||node_id FROM clone_shingles WHERE view_gen<>0 AND repo_prefix='" + repo + "'"},
		{Table: "clone_corpus_state", Select: "SELECT 'clone_corpus gen'||CAST(view_gen AS TEXT)||' '||repo_prefix FROM clone_corpus_state WHERE view_gen<>0 AND repo_prefix='" + repo + "'"},
		{Table: "symbol_fts_rowid", Select: "SELECT 'symbol_fts gen'||CAST(view_gen AS TEXT)||' '||node_id FROM symbol_fts_rowid WHERE view_gen<>0 AND repo_prefix='" + repo + "'"},
		// content_fts_rowid is keyed by the FTS docid and the FILE, not by a
		// node: its columns are fts_rowid / repo_prefix / file_path
		// (internal/graph/store_sqlite/schema.go:1486-1500). Selecting node_id
		// here made the whole UNION unreadable, which is why the run now fails
		// on an unreadable projection instead of reporting it as an empty one.
		{Table: "content_fts_rowid", Select: "SELECT 'content_fts gen'||CAST(view_gen AS TEXT)||' '||file_path||' rowid='||CAST(fts_rowid AS TEXT) FROM content_fts_rowid WHERE view_gen<>0 AND repo_prefix='" + repo + "'"},
		{Table: "generation_edge_sources", Select: "SELECT DISTINCT 'edge_source gen'||CAST(view_gen AS TEXT)||' '||source_id||' '||ownership_mode FROM generation_edge_sources WHERE view_gen<>0"},
		{Table: "generation_producer_completeness", Select: "SELECT 'producer gen'||CAST(view_gen AS TEXT)||' '||producer||' '||state FROM generation_producer_completeness WHERE view_gen<>0"},
	}
}

func e2eResolutionNonServedQuery() string {
	arms := e2eResolutionNonServedArms()
	parts := make([]string, 0, len(arms))
	for _, arm := range arms {
		parts = append(parts, arm.Select)
	}
	return strings.Join(parts, " UNION ")
}

// e2eResolutionTablePattern finds the tables a query reads. Both FROM and JOIN, so a
// projection that reaches a sidecar through a join is covered too.
var e2eResolutionTablePattern = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([a-z_][a-z0-9_]*)`)

// e2eResolutionQueryTables lists the tables one projection reads, deduplicated, in
// first-appearance order.
func e2eResolutionQueryTables(query string) []string {
	var tables []string
	seen := map[string]bool{}
	for _, match := range e2eResolutionTablePattern.FindAllStringSubmatch(query, -1) {
		table := strings.ToLower(match[1])
		if seen[table] {
			continue
		}
		seen[table] = true
		tables = append(tables, table)
	}
	return tables
}

// Two isolated fixtures over the same tree legitimately disagree about exactly
// two things: their Git object names and where on disk they live. Everything
// else is a fact about the tree, and a fact that differs is a divergence.
//
// e2eResolutionFixtureRootPattern matches the shared fixture's own temporary root
// (newIssue767FixtureWithCorpus creates it with the "gx767-" prefix), which
// appears inside recorded error strings such as a read failure's message.
var (
	e2eResolutionShaPattern         = regexp.MustCompile(`\b[0-9a-f]{40,64}\b`)
	e2eResolutionFixtureRootPattern = regexp.MustCompile(`/\S*gx767-\d+`)
)

func e2eResolutionNormalizeRow(row string) string {
	row = e2eResolutionFixtureRootPattern.ReplaceAllString(row, "<fixture-root>")
	return e2eResolutionShaPattern.ReplaceAllString(row, "<sha>")
}

// e2eResolutionReadProjections runs every projection against one store. A table a
// schema does not have yields a nil slice and a recorded note rather than an
// error: a projection that cannot be read must not look like an empty one.
func e2eResolutionReadProjections(ctx context.Context, db *sql.DB, outputs []e2eResolutionOutput) (map[string][]string, map[string]string) {
	rows := map[string][]string{}
	notes := map[string]string{}
	for _, output := range outputs {
		values, err := e2eResolutionQueryRows(ctx, db, output.Query)
		if err != nil {
			notes[output.ID] = err.Error()
			continue
		}
		rows[output.ID] = values
	}
	return rows, notes
}

// e2eResolutionUnreadableProjections renders every projection that could not be READ,
// per arm. A projection whose query errors returns no rows, and a projection
// with no rows is reported as "empty on both arms and so exercised nothing" —
// so an unreadable one would be indistinguishable from a corpus that produced
// nothing. It is not: it is a defect in this file, and the run fails on it.
//
// This is not hypothetical. A recorded arm of nonserved_generation_markers
// selected node_id from content_fts_rowid, which is keyed by fts_rowid and
// file_path; the UNION failed, the projection wrote zero rows, and the artifact
// looked exactly like a generation that had produced nothing.
func e2eResolutionUnreadableProjections(byArm map[string]map[string]string) []string {
	var unreadable []string
	for arm, notes := range byArm {
		for id, note := range notes {
			unreadable = append(unreadable, fmt.Sprintf("%s: projection %s could not be read: %s", arm, id, note))
		}
	}
	sort.Strings(unreadable)
	return unreadable
}

func e2eResolutionQueryRows(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := db.QueryContext(queryCtx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = result.Close() }()
	values := []string{}
	for result.Next() {
		var value sql.NullString
		if err := result.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, e2eResolutionNormalizeRow(value.String))
	}
	if err := result.Err(); err != nil {
		return nil, err
	}
	sort.Strings(values)
	return values, nil
}

// e2eResolutionDerivedSummary says what the oracle comparison actually did with each of
// the ten derived outputs, and keeps "compared" honestly separate from
// "exercised".
//
// Comparing a projection that is byte-identical on both arms is not evidence
// that the reuse path preserves it — it is only evidence that this corpus never
// made it move. The three buckets below are reported separately so a reader
// cannot mistake one for the other: Differing is where the oracle earned its
// keep, Identical is where it found nothing to earn, and Empty is where the
// corpus produced no rows at all on either arm.
type e2eResolutionDerivedSummary struct {
	Differing []string
	Identical []string
	Empty     []string
}

func e2eResolutionSummariseDerived(fresh, incremental map[string][]string, outputs []e2eResolutionOutput) e2eResolutionDerivedSummary {
	var summary e2eResolutionDerivedSummary
	for _, output := range outputs {
		if !output.Derived {
			continue
		}
		freshRows, incrementalRows := fresh[output.ID], incremental[output.ID]
		switch {
		case len(freshRows) == 0 && len(incrementalRows) == 0:
			summary.Empty = append(summary.Empty, output.ID)
		case len(e2eResolutionSetDifference(freshRows, incrementalRows)) == 0 && len(e2eResolutionSetDifference(incrementalRows, freshRows)) == 0:
			summary.Identical = append(summary.Identical, output.ID)
		default:
			summary.Differing = append(summary.Differing, output.ID)
		}
	}
	return summary
}

// Compared is every derived output that carried rows on at least one arm.
func (s e2eResolutionDerivedSummary) Compared() int { return len(s.Differing) + len(s.Identical) }

func (s e2eResolutionDerivedSummary) Describe() string {
	return fmt.Sprintf("ten derived outputs: %d compared (of which %d actually differ between the arms: %v, and %d were identical on both and so exercised nothing on this corpus: %v); %d empty on both arms: %v",
		s.Compared(), len(s.Differing), s.Differing, len(s.Identical), s.Identical, len(s.Empty), s.Empty)
}

// e2eResolutionDivergence is one projection's disagreement between the incrementally
// reached view and a fresh isolated index of the same tree.
type e2eResolutionDivergence struct {
	Output  string   `json:"output"`
	Missing []string `json:"missing,omitempty"` // in the fresh index, absent incrementally
	Extra   []string `json:"extra,omitempty"`   // present incrementally, not in the fresh index
}

// e2eResolutionCompare is the gate-1 oracle. It never compares counts: it compares the
// facts themselves and reports which ones differ.
func e2eResolutionCompare(fresh, incremental map[string][]string, outputs []e2eResolutionOutput) []e2eResolutionDivergence {
	var divergences []e2eResolutionDivergence
	for _, output := range outputs {
		if output.RecordOnly {
			continue
		}
		missing := e2eResolutionSetDifference(fresh[output.ID], incremental[output.ID])
		extra := e2eResolutionSetDifference(incremental[output.ID], fresh[output.ID])
		if len(missing) == 0 && len(extra) == 0 {
			continue
		}
		divergences = append(divergences, e2eResolutionDivergence{Output: output.ID, Missing: missing, Extra: extra})
	}
	return divergences
}

func e2eResolutionSetDifference(left, right []string) []string {
	index := make(map[string]int, len(right))
	for _, row := range right {
		index[row]++
	}
	var only []string
	for _, row := range left {
		if index[row] > 0 {
			index[row]--
			continue
		}
		only = append(only, row)
	}
	sort.Strings(only)
	return only
}

// ---------------------------------------------------------------------------
// Declared gaps.
// ---------------------------------------------------------------------------

// e2eResolutionKnownGap is a divergence this branch is known not to handle. It is
// declared here, printed in the outcome table, and re-checked every run: an
// undeclared divergence fails the run, and a declared gap that stops
// reproducing ALSO fails it, so the list cannot rot into an allowlist.
type e2eResolutionKnownGap struct {
	ID        string
	Output    string
	Direction string // "missing" or "extra"
	Match     string // substring of the diverging row
	Why       string
	Ledger    string
}

// The gaps observed on this branch at the source identity recorded in the
// report for this item. All three are the same defect seen through three
// projections: a comment-only re-parse of a producer file drops the resolved
// edges that the UNCHANGED consumer file owns, and nothing re-derives them —
// the served view_gen 0 keeps the `calls` edge (the restub path restores it)
// but loses the resolved `imports` edge and the `value_flow` edge, both of
// which a fresh isolated index of the same tree has.
//
// The "caused by the re-parse" half of each Why is NOT taken on trust. The
// oracle at stage 8 runs after a whole mutation sequence and cannot separate
// one edit from another, so e2eResolutionStageReparse snapshots these projections
// either side of the comment-only edit alone and e2eResolutionUnattributedGaps fails
// the run for any gap whose rows that isolated measurement did not drop.
func e2eResolutionKnownGaps() []e2eResolutionKnownGap {
	return []e2eResolutionKnownGap{
		{
			ID: "imports_edge_dropped_on_producer_reparse", Output: "incoming_edges_from_context_files",
			Direction: "missing", Match: " -imports-> ",
			Why:    "a comment-only re-parse of the imported file, measured in isolation before any other mutation, drops the importer's resolved imports edge from the served generation and nothing re-derives it",
			Ledger: e2eResolutionLedgerRow,
		},
		{
			ID: "value_flow_edge_dropped_on_producer_reparse", Output: "capability_dataflow_framework_edges",
			Direction: "missing", Match: " -value_flow-> ",
			Why:    "the dataflow edge produced after resolution is not re-derived when only the producer's file is re-parsed; the re-parse stage measures that loss on its own, before the withdraw",
			Ledger: e2eResolutionLedgerRow,
		},
		{
			// The same lost edge, seen through the provenance projection: a
			// value_flow edge carries origin=ast_resolved, so its loss is a
			// provenance row's loss as well. One defect, two lenses — declared
			// twice so neither lens silently absorbs the other's rows.
			ID: "value_flow_provenance_dropped_on_producer_reparse", Output: "restub_provenance",
			Direction: "missing", Match: " -value_flow-> ",
			Why:    "the same dropped dataflow edge is also the provenance row it carried, and it goes at the same isolated re-parse",
			Ledger: e2eResolutionLedgerRow,
		},
	}
}

// e2eResolutionGapProjections is the set of projections the declared gaps name, in
// e2eResolutionOutputs order. It is what the re-parse stage snapshots, so the causal
// claim is measured on exactly the observables the gaps are declared against.
func e2eResolutionGapProjections() []e2eResolutionOutput {
	wanted := map[string]bool{}
	for _, gap := range e2eResolutionKnownGaps() {
		wanted[gap.Output] = true
	}
	var outputs []e2eResolutionOutput
	for _, output := range e2eResolutionOutputs() {
		if wanted[output.ID] {
			outputs = append(outputs, output)
		}
	}
	return outputs
}

// e2eResolutionReparseDelta is what ONE mutation did to the gap projections, measured
// arm-against-itself. It is the isolation the oracle cannot provide: the oracle
// compares two trees after a whole sequence, this compares one store either
// side of a single edit.
type e2eResolutionReparseDelta struct {
	Lost   map[string][]string `json:"lost,omitempty"`
	Gained map[string][]string `json:"gained,omitempty"`
	// Moved is the same fact at a new location. The comment-only revision
	// pushes the declaration from line 3 to line 5, and every projection that
	// renders "@file:line" therefore reports the old row gone and a new one
	// arrived. That is the edit doing exactly what it was written to do, not a
	// lost fact, and counting it as a loss would make the provenance assertion
	// fail for the one reason it must not.
	Moved map[string][]string `json:"moved,omitempty"`
}

// e2eResolutionLocationPattern matches the " @<path>:<line>" segment every located
// projection renders. Stripping it leaves the fact — the edge's endpoints, its
// kind and its provenance — which is what "the same row at a new line" means.
var e2eResolutionLocationPattern = regexp.MustCompile(` @[^ ]*:\d+`)

func e2eResolutionStripLocation(row string) string {
	return e2eResolutionLocationPattern.ReplaceAllString(row, "")
}

func e2eResolutionReparseDeltaOf(before, after map[string][]string, outputs []e2eResolutionOutput) e2eResolutionReparseDelta {
	delta := e2eResolutionReparseDelta{Lost: map[string][]string{}, Gained: map[string][]string{}, Moved: map[string][]string{}}
	for _, output := range outputs {
		lost := e2eResolutionSetDifference(before[output.ID], after[output.ID])
		gained := e2eResolutionSetDifference(after[output.ID], before[output.ID])

		arrived := map[string]int{}
		for _, row := range gained {
			arrived[e2eResolutionStripLocation(row)]++
		}
		var reallyLost, moved []string
		for _, row := range lost {
			key := e2eResolutionStripLocation(row)
			if arrived[key] > 0 {
				arrived[key]--
				moved = append(moved, row)
				continue
			}
			reallyLost = append(reallyLost, row)
		}
		var reallyGained []string
		for _, row := range gained {
			key := e2eResolutionStripLocation(row)
			if arrived[key] > 0 {
				arrived[key]--
				reallyGained = append(reallyGained, row)
			}
		}
		sort.Strings(reallyLost)
		sort.Strings(reallyGained)
		sort.Strings(moved)
		if len(reallyLost) > 0 {
			delta.Lost[output.ID] = reallyLost
		}
		if len(reallyGained) > 0 {
			delta.Gained[output.ID] = reallyGained
		}
		if len(moved) > 0 {
			delta.Moved[output.ID] = moved
		}
	}
	return delta
}

// e2eResolutionUnattributedGaps returns the declared gaps whose stated cause — the
// comment-only re-parse — this measurement does not support.
//
// A gap declared "missing" claims the re-parse drops a row; the evidence is
// that row disappearing across the re-parse alone. A gap declared "extra"
// claims the re-parse invents one; the evidence is the mirror. A gap with
// neither is carrying a causal claim the run never established, and the Why
// string has to be corrected or the gap re-measured — the point of this check
// is that a plausible-sounding cause cannot ride along unexamined.
func e2eResolutionUnattributedGaps(delta e2eResolutionReparseDelta, gaps []e2eResolutionKnownGap) []string {
	var unattributed []string
	for _, gap := range gaps {
		rows := delta.Lost[gap.Output]
		if gap.Direction == "extra" {
			rows = delta.Gained[gap.Output]
		}
		found := false
		for _, row := range rows {
			if strings.Contains(row, gap.Match) {
				found = true
				break
			}
		}
		if !found {
			unattributed = append(unattributed, fmt.Sprintf("%s (%s/%s %q)", gap.ID, gap.Output, gap.Direction, gap.Match))
		}
	}
	sort.Strings(unattributed)
	return unattributed
}

// e2eResolutionClassification is the verdict over one oracle comparison.
type e2eResolutionClassification struct {
	// Matched maps a declared gap ID to the rows it explained.
	Matched map[string][]string
	// Unmatched is every diverging row no declared gap explains. One of these
	// fails the run.
	Unmatched []string
	// Stale is every declared gap that explained nothing this run. One of
	// these fails the run too: the entry has to go and the ledger row closed.
	Stale []string
}

func e2eResolutionClassify(divergences []e2eResolutionDivergence, gaps []e2eResolutionKnownGap) e2eResolutionClassification {
	result := e2eResolutionClassification{Matched: map[string][]string{}}
	for _, divergence := range divergences {
		for _, pair := range []struct {
			direction string
			rows      []string
		}{{"missing", divergence.Missing}, {"extra", divergence.Extra}} {
			for _, row := range pair.rows {
				gap, ok := e2eResolutionMatchGap(divergence.Output, pair.direction, row, gaps)
				if !ok {
					result.Unmatched = append(result.Unmatched, fmt.Sprintf("%s/%s: %s", divergence.Output, pair.direction, row))
					continue
				}
				result.Matched[gap.ID] = append(result.Matched[gap.ID], fmt.Sprintf("%s: %s", pair.direction, row))
			}
		}
	}
	for _, gap := range gaps {
		if len(result.Matched[gap.ID]) == 0 {
			result.Stale = append(result.Stale, gap.ID)
		}
	}
	sort.Strings(result.Unmatched)
	sort.Strings(result.Stale)
	return result
}

func e2eResolutionMatchGap(output, direction, row string, gaps []e2eResolutionKnownGap) (e2eResolutionKnownGap, bool) {
	for _, gap := range gaps {
		if gap.Output == output && gap.Direction == direction && strings.Contains(row, gap.Match) {
			return gap, true
		}
	}
	return e2eResolutionKnownGap{}, false
}

// ---------------------------------------------------------------------------
// Fixture wiring.
// ---------------------------------------------------------------------------

func e2eResolutionConfigPath(f *issue767Fixture) string {
	return filepath.Join(f.root, "config", "gortex", "config.yaml")
}

// e2eResolutionConfigYAML tracks both repositories. It deliberately does NOT carry
// index.max_file_size: GetRepoConfig (internal/config/manager.go:281-286) reads
// a repository's OWN .gortex.yaml or compiled defaults and never this file's
// `index:` block, so a cap written here would be reported by the run and absent
// from the indexer. The cap travels in the corpus instead
// (e2eResolutionWorkspaceConfig), which also means the fresh oracle's copied tree
// carries it without any extra wiring.
func e2eResolutionConfigYAML(f *issue767Fixture) string {
	other := filepath.Join(f.root, e2eResolutionOtherRepoDir)
	return "repos:\n" +
		"  - path: " + strconv.Quote(f.primary) + "\n    name: " + issue767FixturePrefix + "\n" +
		"  - path: " + strconv.Quote(other) + "\n    name: " + e2eResolutionOtherRepoName + "\n"
}

// e2eResolutionSetup builds one isolated fixture over the supplied primary tree, adds
// the second repository, rewrites the private configuration and applies the
// unreadable mode. It does not start the daemon.
func e2eResolutionSetup(t *testing.T, binary string, primary []e2eResolutionSourceFile) *issue767Fixture {
	t.Helper()
	f := newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		for _, file := range primary {
			f.write(filepath.Join(f.primary, filepath.FromSlash(file.Path)), file.Content)
		}
		for _, file := range e2eResolutionOtherRepoFiles() {
			f.write(filepath.Join(f.root, e2eResolutionOtherRepoDir, filepath.FromSlash(file.Path)), file.Content)
		}
	})
	other := filepath.Join(f.root, e2eResolutionOtherRepoDir)
	f.git(other, "init", "-b", "main")
	f.git(other, "add", ".")
	f.git(other, "commit", "-m", "resolution matrix cross-repository producer control")
	f.write(e2eResolutionConfigPath(f), e2eResolutionConfigYAML(f))
	for _, file := range primary {
		if !file.Unreadable {
			continue
		}
		if err := os.Chmod(filepath.Join(f.primary, filepath.FromSlash(file.Path)), 0); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// e2eResolutionCopyTree reads a fixture's primary tree back out so a second fixture can
// be built over exactly the same bytes. The unreadable manifest cannot be read
// by definition: it is carried through from the declared corpus, with its mode
// preserved, so the oracle's tree is the same tree.
func e2eResolutionCopyTree(t *testing.T, root string) []e2eResolutionSourceFile {
	t.Helper()
	declared := map[string]e2eResolutionSourceFile{}
	for _, file := range e2eResolutionCorpusFiles() {
		declared[file.Path] = file
	}
	var files []e2eResolutionSourceFile
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		slash := filepath.ToSlash(rel)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			original, ok := declared[slash]
			if !ok {
				return readErr
			}
			files = append(files, original)
			return nil
		}
		files = append(files, e2eResolutionSourceFile{Path: slash, Content: string(data), Unreadable: declared[slash].Unreadable})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// ---------------------------------------------------------------------------
// Store probes used by the staged cases.
// ---------------------------------------------------------------------------

func e2eResolutionRows(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("resolution matrix query %q: %v", query, err)
	}
	defer func() { _ = result.Close() }()
	var values []string
	for result.Next() {
		var value sql.NullString
		if err := result.Scan(&value); err != nil {
			t.Fatalf("resolution matrix scan %q: %v", query, err)
		}
		values = append(values, value.String)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("resolution matrix rows %q: %v", query, err)
	}
	sort.Strings(values)
	return values
}

// e2eResolutionSizeSkipMarker is the meta key sizeSkipNode writes
// (internal/indexer/skip_telemetry.go:199-215). It is searched for as raw
// bytes inside nodes.meta: the flat meta codec stores a key as
// [uvarint len][key bytes] (internal/graph/store_sqlite/meta_json.go:393-397,
// 466-470), so the key's own bytes appear verbatim in the blob.
const e2eResolutionSizeSkipMarker = "skipped_due_to_size"

// e2eResolutionSizeSkipRows reads one file node together with the size-skip telemetry
// probe, rendered as "<id> kind=<kind> lang=<lang> size_skip=<0|1>".
//
// The probe is what separates a size-skipped file from an ordinary one: a file
// node with no symbols is NOT evidence of the cap (run 7 produced exactly that
// row for a fully parsed 2 MiB file). instr() over two BLOBs is a byte search,
// which is why the marker is cast rather than compared as text — the meta blob
// begins with a 0x00 magic byte and would not survive a TEXT cast.
func e2eResolutionSizeSkipRows(t *testing.T, db *sql.DB, id string) []string {
	t.Helper()
	return e2eResolutionRows(t, db,
		"SELECT id||' kind='||kind||' lang='||language||' size_skip='||CAST(CASE WHEN instr(COALESCE(meta,CAST('' AS BLOB)),CAST(? AS BLOB))>0 THEN 1 ELSE 0 END AS TEXT) "+
			"FROM nodes WHERE view_gen=0 AND id=?", e2eResolutionSizeSkipMarker, id)
}

// e2eResolutionSizeSkipMarked reports whether a probed row carries the size-skip
// telemetry marker.
func e2eResolutionSizeSkipMarked(rows []string) bool {
	for _, row := range rows {
		if strings.HasSuffix(row, " size_skip=1") {
			return true
		}
	}
	return false
}

// e2eResolutionMixedVerdict judges the mixed-language edges on their TARGETS.
//
// named reports whether the exact cross-language bind the corpus names is
// present; decoyBinds is every edge that landed on the same-named symbol in the
// other language instead. Judging the rendered row as a whole is what made the
// previous shape unfalsifiable: the query pins from_id, so any predicate over
// the source half is already guaranteed.
func e2eResolutionMixedVerdict(rows []string, want, decoyPrefix string) (named bool, decoyBinds []string) {
	for _, row := range rows {
		if row == want {
			named = true
		}
		_, target, _ := e2eResolutionDynamicEdgeParts(row)
		if decoyPrefix != "" && strings.HasPrefix(target, decoyPrefix) {
			decoyBinds = append(decoyBinds, row)
		}
	}
	sort.Strings(decoyBinds)
	return named, decoyBinds
}

// e2eResolutionEdgeRow is one edge's full provenance row, or "" when the edge is not in
// the served generation. It is the observable for restub identity+provenance.
func e2eResolutionEdgeRow(t *testing.T, db *sql.DB, from, to, kind string) string {
	t.Helper()
	rows := e2eResolutionRows(t, db,
		"SELECT from_id||' -'||kind||'-> '||to_id||' @'||file_path||':'||CAST(line AS TEXT)||' origin='||origin||' tier='||tier||' conf='||CAST(confidence AS TEXT)||' label='||confidence_label "+
			"FROM edges WHERE view_gen=0 AND from_id=? AND to_id=? AND kind=?", from, to, kind)
	return strings.Join(rows, " | ")
}

// e2eResolutionAwaitNodeLine waits until the served generation declares a node at a
// given start line — the direct observation that a re-parse landed, for an edit
// that deliberately declares no new symbol.
func e2eResolutionAwaitNodeLine(f *issue767Fixture, db *sql.DB, id string, line int64) {
	f.t.Helper()
	f.await(fmt.Sprintf("node %s at line %d", id, line), 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
		defer cancel()
		var start int64
		if err := db.QueryRowContext(ctx, "SELECT start_line FROM nodes WHERE view_gen=0 AND id=?", id).Scan(&start); err != nil {
			return false
		}
		return start == line
	})
}

func e2eResolutionAwaitNodeGone(f *issue767Fixture, db *sql.DB, id string) {
	f.t.Helper()
	f.await("node "+id+" withdrawn", 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
		defer cancel()
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE view_gen=0 AND id=?", id).Scan(&count); err != nil {
			return false
		}
		return count == 0
	})
}

// e2eResolutionFileMtime is the served generation's recorded mtime for one repository
// path. It is the observable for a manifest-only edit, which by design changes
// no symbol and therefore no node.
func e2eResolutionFileMtime(t *testing.T, db *sql.DB, rel string) string {
	t.Helper()
	rows := e2eResolutionRows(t, db, "SELECT CAST(mtime_ns AS TEXT) FROM file_mtimes WHERE view_gen=0 AND repo_prefix=? AND file_path=?", issue767FixturePrefix, rel)
	return strings.Join(rows, ",")
}

func e2eResolutionAwaitFileMtimeChange(f *issue767Fixture, db *sql.DB, rel, previous string) bool {
	f.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if current := e2eResolutionFileMtime(f.t, db, rel); current != previous && current != "" {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// ---------------------------------------------------------------------------
// The opt-in matrix run.
// ---------------------------------------------------------------------------

// e2eResolutionRequiredTimeout is the wall clock the staged run needs: two cold indexes
// plus nine settles at the fixture's own three-stable-samples cadence.
func e2eResolutionRequiredTimeout() time.Duration { return 25 * time.Minute }

func TestE2EMatrixResolutionProvenanceManifests(t *testing.T) {
	binary := os.Getenv(e2eResolutionBinaryEnv)
	if binary == "" {
		t.Skip(e2eResolutionSkipReason)
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	if deadline, ok := t.Deadline(); ok && time.Until(deadline) < e2eResolutionRequiredTimeout() {
		t.Fatalf("the resolution matrix needs at least %s remaining; run with go test -timeout %s or longer", e2eResolutionRequiredTimeout(), e2eResolutionRequiredTimeout())
	}
	artifactDir := os.Getenv(e2eResolutionArtifactEnv)
	if artifactDir == "" {
		artifactDir = t.TempDir()
		t.Logf("%s is unset; artifacts go to %s and are removed with the test", e2eResolutionArtifactEnv, artifactDir)
	}
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}

	rec := e2eResolutionNewRecorder(t)
	defer func() {
		table := e2eResolutionRenderOutcomes(rec.rows)
		t.Log("\n" + table)
		if err := os.WriteFile(filepath.Join(artifactDir, "outcomes.txt"), []byte(table), 0o600); err != nil {
			t.Errorf("resolution matrix: writing the outcome table: %v", err)
		}
		for _, problem := range e2eResolutionValidateOutcomes(e2eResolutionCases(), rec.rows) {
			t.Errorf("resolution matrix outcome table: %s", problem)
		}
	}()

	arm := e2eResolutionSetup(t, binary, e2eResolutionCorpusFiles())
	t.Logf("resolution matrix incremental arm root: %s", arm.root)
	arm.start()
	db := arm.openReadOnly()
	arm.awaitSymbolIn(arm.primary, "Issue767PrimaryMarker", filepath.Join(arm.primary, "marker.go"), 3*time.Minute)
	arm.settle()

	e2eResolutionStageCold(t, rec, arm, db)
	e2eResolutionStageCrossRepository(t, rec, arm, db)
	e2eResolutionStageDecoyEdit(t, rec, arm, db)
	e2eResolutionStageManifestOnly(t, rec, arm, db)
	before := e2eResolutionStageReparse(t, rec, arm, db)
	e2eResolutionStageWithdraw(t, rec, arm, db)
	e2eResolutionStageRestore(t, rec, arm, db, before)
	e2eResolutionStageProviderEvidence(t, rec, arm, db)
	e2eResolutionStageOracle(t, rec, arm, db, binary, artifactDir)
}

// Stage 0 — what the cold index made of every manifest shape.
func e2eResolutionStageCold(t *testing.T, rec *e2eResolutionRecorder, f *issue767Fixture, db *sql.DB) {
	t.Helper()

	realBind := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	rec.assert("real_imported_consumer_binds_its_own_module", realBind != "",
		"consumer→producer calls edge in the served generation: %q", realBind)

	decoyBind := e2eResolutionRows(t, db, "SELECT from_id||' -'||kind||'-> '||to_id FROM edges WHERE view_gen=0 AND to_id=? AND from_id LIKE ?",
		e2eResolutionDecoyProduceID, issue767FixturePrefix+"/consumer/%")
	// The same-named symbol the consumer must NOT be bound to has to exist, or
	// "nothing binds to it" is a fact about an empty directory. It does exist
	// on this corpus — and the withdraw stage later observes this branch
	// binding the consumer to exactly this node, which is what makes the
	// absence here a real observation rather than a guaranteed one.
	decoyProduce := e2eResolutionRows(t, db, "SELECT id||' kind='||kind FROM nodes WHERE view_gen=0 AND id=?", e2eResolutionDecoyProduceID)
	rec.assert("unrelated_same_named_module_is_not_bound", len(decoyBind) == 0 && len(decoyProduce) == 1 && realBind != "",
		"edges from the consumer into the unrelated same-named module: %v; the same-named decoy symbol an edge COULD have named: %v; the consumer's real binding: %q",
		decoyBind, decoyProduce, realBind)

	decoyOwner := e2eResolutionRows(t, db, "SELECT repo_prefix||' '||file_path FROM nodes WHERE view_gen=0 AND id=?", e2eResolutionDecoyProduceID)
	crossModule := e2eResolutionRows(t, db, "SELECT from_id||' -'||kind||'-> '||to_id FROM edges WHERE view_gen=0 AND to_id LIKE ? AND from_id NOT LIKE ?",
		issue767FixturePrefix+"/decoy/%", issue767FixturePrefix+"/decoy/%")
	// The nested module's own source file has to BE in the graph, or "nothing
	// crosses into it" is true of an empty directory rather than of a module
	// boundary. It is also the control the vendor case borrows below.
	decoyNodes := e2eResolutionRows(t, db, "SELECT id FROM nodes WHERE view_gen=0 AND id LIKE ?", issue767FixturePrefix+"/decoy/producer/%")
	rec.assert("nested_manifest_module_root_mismatch", len(crossModule) == 0 && len(decoyNodes) > 0,
		"nested module owner %v; its own indexed nodes (the control: the directory is not simply empty) %v; edges crossing into it from outside: %v",
		decoyOwner, decoyNodes, crossModule)

	vendorBind := e2eResolutionRows(t, db, "SELECT from_id||' -'||kind||'-> '||to_id FROM edges WHERE view_gen=0 AND to_id LIKE ? AND from_id NOT LIKE ?",
		issue767FixturePrefix+"/vendor/%", issue767FixturePrefix+"/vendor/%")
	vendorNodes := e2eResolutionRows(t, db, "SELECT id FROM nodes WHERE view_gen=0 AND id LIKE ?", issue767FixturePrefix+"/vendor/%")
	// "No bind into vendor" is only an observation while there is something an
	// edge could have named. The vendored tree is excluded before resolution,
	// so when it contributes NO node the empty result is guaranteed by the
	// exclusion and the assertion could not come out false at any corpus — the
	// disproving control (the equally nested, equally same-named, NOT vendored
	// decoy directory, which IS indexed) shows the corpus is not simply empty,
	// but it cannot make the vendored target exist. That case is recorded
	// not_exercised with the measurement, not passed.
	//
	// When the vendored tree IS in the graph the assertion is real: the
	// vendored copy of Produce is then a node the same-name rebind this matrix
	// observes elsewhere (stage 4, the decoy) could land on, and the case
	// asserts it did not, alongside the real binding it must have kept.
	if len(vendorNodes) == 0 {
		rec.skipMeasured("vendor_manifest", fmt.Sprintf(
			"the vendored tree contributed no node to the served generation (%v), so no edge could name a vendored symbol and an empty result is guaranteed by the exclusion rather than observed; control — the equally nested non-vendored decoy directory contributed %v, and the real consumer binding is %q",
			vendorNodes, decoyNodes, realBind))
	} else {
		rec.assert("vendor_manifest", len(vendorBind) == 0 && len(decoyNodes) > 0 && realBind != "",
			"vendored nodes in the served generation: %v — so an edge into the vendored copy was possible; edges binding into vendor from outside it: %v; control — the equally nested non-vendored decoy directory contributed %v; the real consumer binding is %q",
			vendorNodes, vendorBind, decoyNodes, realBind)
	}

	siblingsOfInvalid := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	markerNode := e2eResolutionRows(t, db, "SELECT id FROM nodes WHERE view_gen=0 AND id=?", e2eResolutionMarkerID)
	// The subject of this case is the SIBLING: a source file in the very
	// directory whose manifest cannot be parsed. Without it the case would be
	// observing files in unrelated directories and calling that a result.
	brokenSibling := e2eResolutionRows(t, db, "SELECT id||' kind='||kind FROM nodes WHERE view_gen=0 AND id=?", e2eResolutionBrokenSiblingID)
	rec.assert("invalid_manifest", len(brokenSibling) == 1 && siblingsOfInvalid != "" && len(markerNode) == 1,
		"the source file in the unparseable manifest's OWN directory: %v; the pass also still produced the marker %v and the real binding %q",
		brokenSibling, markerNode, siblingsOfInvalid)

	// The manifest half of the oversized bullet cannot fail either way on this
	// branch, and is declared so in source rather than recorded as a pass. The
	// control is measured, not assumed: a nested manifest earns no node, so
	// the 2 MiB manifest and a 1 KiB one are indistinguishable observations.
	oversizedManifestNodes := e2eResolutionRows(t, db, "SELECT id FROM nodes WHERE view_gen=0 AND id LIKE ?", issue767FixturePrefix+"/oversized/%")
	smallNestedManifestNodes := e2eResolutionRows(t, db, "SELECT id FROM nodes WHERE view_gen=0 AND id=?", issue767FixturePrefix+"/decoy/go.mod")
	rec.skipDeclared("oversized_manifest", fmt.Sprintf(
		"index.max_file_size=%d; nodes from the %d-byte nested manifest oversized/go.mod: %v; nodes from the readable %d-byte nested manifest decoy/go.mod: %v — the same silence at both sizes",
		e2eResolutionMaxFileSize, e2eResolutionOversizedBytes, oversizedManifestNodes, e2eResolutionCorpusFileSize("decoy/go.mod"), smallNestedManifestNodes))

	// The oversized arm that CAN fail. big.go is over the cap, small.go beside
	// it is under it, both are Go and so both reach the cap at
	// internal/indexer/walk_source.go:88. An indexer that ignored the cap
	// would produce GxResolutionOversizedValue exactly as it produces the control.
	//
	// "A file node with no symbols" is NOT on its own a size-skip: run 7
	// produced the identical file-node row while the file was fully parsed.
	// The discriminating observable is the telemetry sizeSkipNode writes
	// (internal/indexer/skip_telemetry.go:199-215 — skip_reason /
	// skipped_due_to_size / file_size_bytes / max_file_size_bytes), which
	// rides in nodes.meta. meta is the flat binary codec
	// (internal/graph/store_sqlite/meta_json.go:391-420): a uvarint-prefixed
	// key is stored as its raw bytes, so a byte search for the key is a sound
	// probe, and the in-spec control's own file node is the proof that the
	// probe discriminates instead of answering the same thing for every file.
	bigStub := e2eResolutionSizeSkipRows(t, db, e2eResolutionBigSourceFileID)
	bigSymbols := e2eResolutionRows(t, db, "SELECT id||' kind='||kind FROM nodes WHERE view_gen=0 AND id LIKE ?", e2eResolutionBigSourceFileID+"::%")
	controlSymbols := e2eResolutionRows(t, db, "SELECT id||' kind='||kind FROM nodes WHERE view_gen=0 AND id=?", e2eResolutionBigControlSymID)
	controlStub := e2eResolutionSizeSkipRows(t, db, e2eResolutionBigControlFileID)
	rec.assert("oversized_source_is_size_skipped",
		len(bigStub) == 1 && e2eResolutionSizeSkipMarked(bigStub) && len(bigSymbols) == 0 &&
			len(controlSymbols) == 1 && len(controlStub) == 1 && !e2eResolutionSizeSkipMarked(controlStub),
		"index.max_file_size=%d; the %d-byte source file's node, with the size-skip telemetry probe: %v; symbols extracted from it: %v; the in-spec control file beside it: %v; the control's OWN file node and probe (it must NOT carry the marker, or the probe discriminates nothing): %v",
		e2eResolutionMaxFileSize, len(e2eResolutionOversizedSource()), bigStub, bigSymbols, controlSymbols, controlStub)

	switch {
	case runtime.GOOS == "windows":
		rec.skip("unreadable_manifest", "POSIX mode 0 does not deny a read on this platform, so an unreadable path cannot be created here")
	case os.Geteuid() == 0:
		rec.skip("unreadable_manifest", "the run is root, so mode 0 does not deny a read and the case cannot be exercised on this host")
	default:
		// Scoped exactly like the index_failures projection: the served
		// generation and the primary's own repo_prefix. Unscoped, the probe
		// could be satisfied by a row a NON-served generation or the second
		// repository wrote, which is not the fact the case is about.
		failures := e2eResolutionRows(t, db, "SELECT file_path||' denied='||CAST(permission_denied AS TEXT)||' err='||error FROM file_index_failures WHERE view_gen=0 AND repo_prefix=? AND file_path LIKE ?",
			issue767FixturePrefix, "%unreadable%")
		unreadableNodes := e2eResolutionRows(t, db, "SELECT id||' kind='||kind FROM nodes WHERE view_gen=0 AND id LIKE ?", issue767FixturePrefix+"/unreadable/%")
		nestedManifest := e2eResolutionRows(t, db, "SELECT id FROM nodes WHERE view_gen=0 AND id=?", issue767FixturePrefix+"/decoy/go.mod")
		// A manifest below the repository root earns no node even when it is
		// perfectly readable (nestedManifest is the control), so the manifest
		// arm can only ever observe silence. The source file beside it is the
		// one the pass really reads, and a read it could not complete has to
		// leave evidence rather than an absence.
		rec.assert("unreadable_manifest", len(failures) > 0 || len(unreadableNodes) > 0,
			"control — a readable nested manifest earns nodes %v, so an unreadable one below the root is out of the corpus by construction; recorded read failures: %v; nodes from the unreadable source file: %v",
			nestedManifest, failures, unreadableNodes)
	}

	rubyNodes := e2eResolutionRows(t, db, "SELECT id||' lang='||language FROM nodes WHERE view_gen=0 AND id LIKE ?", issue767FixturePrefix+"/nometa/%")
	rec.assert("missing_metadata", len(rubyNodes) > 0,
		"a file whose language has no manifest anywhere in the tree still produced: %v", rubyNodes)

	// The dynamic case, held to the SAME standard the rest of this matrix
	// applies to origin=text_matched. The previous shape exempted any target
	// inside /dyn/ from being "unknowable", which let the one bind the case
	// exists to judge — importlib.import_module(name) reaching a concrete
	// sibling symbol — through unexamined.
	//
	// The corpus carries both halves on purpose: run() reaches
	// helper_value through a runtime handle (unprovable), call_helper()
	// reaches the SAME symbol through an ordinary `from . import helper`
	// (provable). A graph that cannot tell those two apart — identical kind
	// and identical provenance to the same target — does not degrade a guess
	// truthfully, and that is the assertion.
	dynamicBinds := e2eResolutionDynamicEdgeRows(t, db, e2eResolutionDynDynamicID)
	staticBinds := e2eResolutionDynamicEdgeRows(t, db, e2eResolutionDynStaticID)
	guesses, indistinguishable := e2eResolutionDynamicGuessVerdict(dynamicBinds, staticBinds)
	rec.assert("dynamic_language_conservative", len(staticBinds) > 0 && len(indistinguishable) == 0,
		"edges out of the dynamic entry point (%s): %v; edges out of the static sibling (%s), the control: %v; binds the dynamic import cannot name: %v; of those, indistinguishable from the static sibling's proof of the same target: %v",
		e2eResolutionDynDynamicID, dynamicBinds, e2eResolutionDynStaticID, staticBinds, guesses, indistinguishable)

	// The mixed-language case, judged on the TARGET of each edge.
	//
	// The previous shape filtered rows for the substring "/mixed/" over the
	// WHOLE rendered row, while the query already pinned from_id to
	// issue767/mixed/% — so every row contained it by construction and the
	// filter could never select anything. Two halves replace it, and each can
	// fail on its own:
	//
	//   - the named cross-language bind (the TypeScript entry point calling
	//     the JavaScript function it imports by name) has to BE there;
	//   - no edge out of the pair may land on mixeddecoy/helper.ts, which
	//     exports the SAME name from the OTHER language and is imported by
	//     nothing — the by-name cross-language mis-bind the hazard is about.
	//
	// The decoy symbol's presence is part of the assertion: without it the
	// second half would be a fact about an absent file.
	mixedBinds := e2eResolutionRows(t, db, "SELECT from_id||' -'||kind||'-> '||to_id FROM edges WHERE view_gen=0 AND from_id LIKE ? AND kind IN ("+e2eResolutionResolvedEdgeKinds+")",
		issue767FixturePrefix+"/mixed/%")
	mixedDecoyNode := e2eResolutionRows(t, db, "SELECT id||' lang='||language FROM nodes WHERE view_gen=0 AND id=?", e2eResolutionMixedDecoyID)
	namedBind, decoyBinds := e2eResolutionMixedVerdict(mixedBinds, e2eResolutionMixedNamedBind, e2eResolutionMixedDecoyPfx)
	rec.assert("mixed_language_imports", namedBind && len(decoyBinds) == 0 && len(mixedDecoyNode) == 1,
		"edges out of the TypeScript entry point: %v; the named cross-language bind %q present: %t; the same-named decoy in the other language, which an edge could have landed on instead: %v; edges that DID land on it: %v",
		mixedBinds, e2eResolutionMixedNamedBind, namedBind, mixedDecoyNode, decoyBinds)
}

// Stage 1 — an edit to the unrelated same-named module.
func e2eResolutionStageDecoyEdit(t *testing.T, rec *e2eResolutionRecorder, f *issue767Fixture, db *sql.DB) {
	t.Helper()
	before := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	f.write(filepath.Join(f.primary, "decoy", "producer", "producer.go"), e2eResolutionDecoySource(true))
	f.await("decoy revision indexed", 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes WHERE view_gen=0 AND id=?", e2eResolutionDecoyOnlyID).Scan(&count); err != nil {
			return false
		}
		return count == 1
	})
	f.settle()
	after := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	rec.assert("unrelated_module_edit_does_not_disturb_real_binding", before == after && after != "",
		"real binding before %q; after an edit to the unrelated same-named module %q", before, after)
}

// Stage 2 — a manifest-only edit (H8).
func e2eResolutionStageManifestOnly(t *testing.T, rec *e2eResolutionRecorder, f *issue767Fixture, db *sql.DB) {
	t.Helper()
	beforeMtime := e2eResolutionFileMtime(t, db, e2eResolutionGoModRel)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	beforeGenerations, genErr := issue767ReadGenerations(ctx, db)
	cancel()
	if genErr != nil {
		t.Fatalf("resolution matrix: reading generations before the manifest edit: %v", genErr)
	}
	beforeBind := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")

	f.write(filepath.Join(f.primary, "go.mod"), e2eResolutionRootManifest(true))
	saw := e2eResolutionAwaitFileMtimeChange(f, db, e2eResolutionGoModRel, beforeMtime)
	f.settle()

	afterCtx, afterCancel := context.WithTimeout(t.Context(), 10*time.Second)
	afterGenerations, genErr := issue767ReadGenerations(afterCtx, db)
	afterCancel()
	if genErr != nil {
		t.Fatalf("resolution matrix: reading generations after the manifest edit: %v", genErr)
	}
	afterBind := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	rec.assert("manifest_only_invalidation", beforeBind == afterBind && afterBind != "",
		"manifest-only edit observed=%t; every binding unchanged (%q); payload generations %d→%d, sequence %d→%d — the over-invalidation measurement, not an assertion",
		saw, afterBind, beforeGenerations.Count, afterGenerations.Count, beforeGenerations.Sequence, afterGenerations.Sequence)
}

// Stage 3 — the idempotent re-parse. Returns the provenance row the restore
// stage has to reproduce exactly.
//
// This stage is where the comment-only re-parse is ISOLATED. The oracle at
// stage 8 runs after the withdraw/restore sequence, which demonstrably destroys
// edges of its own, so an oracle divergence on its own cannot say which
// mutation caused it. Every declared gap blames the re-parse; this stage is
// where that claim is measured, before anything else has happened.
func e2eResolutionStageReparse(t *testing.T, rec *e2eResolutionRecorder, f *issue767Fixture, db *sql.DB) string {
	t.Helper()
	gapOutputs := e2eResolutionGapProjections()
	snapshotCtx, snapshotCancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer snapshotCancel()

	before := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	beforeRows, beforeNotes := e2eResolutionReadProjections(snapshotCtx, db, gapOutputs)

	f.write(filepath.Join(f.primary, "producer", "producer.go"), e2eResolutionProducerSource(e2eResolutionProducerCommented))
	e2eResolutionAwaitNodeLine(f, db, e2eResolutionProduceID, e2eResolutionProduceLine(e2eResolutionProducerCommented))
	f.settle()

	after := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	afterRows, afterNotes := e2eResolutionReadProjections(snapshotCtx, db, gapOutputs)
	// An unreadable projection is a defect in this file, not a measurement: it
	// would make the re-parse delta look like "nothing was lost" for a query
	// that never ran.
	for _, problem := range e2eResolutionUnreadableProjections(map[string]map[string]string{"before the re-parse": beforeNotes, "after the re-parse": afterNotes}) {
		t.Errorf("resolution matrix re-parse snapshot: %s; an unreadable projection must not be mistaken for one that lost nothing", problem)
	}
	delta := e2eResolutionReparseDeltaOf(beforeRows, afterRows, gapOutputs)

	// The identity half is the calls edge; the provenance half is checked on
	// rows that actually CARRY provenance. The calls edge's origin/tier/
	// confidence columns are empty on this corpus, so comparing it to itself
	// would be comparing empty to empty and calling that a preserved
	// provenance — the restub_provenance projection is the observable that can
	// actually lose something.
	lostProvenance := delta.Lost["restub_provenance"]
	rec.assert("restub_identity_and_provenance_preserved",
		before == after && after != "" && len(lostProvenance) == 0,
		"incoming edge before the comment-only re-parse %q; after %q (the declaration moved to line %d, so the re-parse did happen); provenance-bearing rows the re-parse ALONE dropped: %v; rows it gained: %v; rows that merely MOVED with the declaration and are not a loss: %v",
		before, after, e2eResolutionProduceLine(e2eResolutionProducerCommented), lostProvenance, delta.Gained["restub_provenance"], delta.Moved["restub_provenance"])

	// And the causal claim every declared gap makes, measured rather than
	// asserted in prose: the re-parse alone has to be what drops the rows.
	unattributed := e2eResolutionUnattributedGaps(delta, e2eResolutionKnownGaps())
	rec.assert("declared_gaps_are_caused_by_the_reparse_alone", len(unattributed) == 0,
		"the comment-only re-parse alone, measured before any withdraw or restore, dropped %v, gained %v and moved %v; declared gaps whose stated cause this measurement does NOT support: %v",
		delta.Lost, delta.Gained, delta.Moved, unattributed)
	return after
}

// Stage 4 — the definition is withdrawn.
func e2eResolutionStageWithdraw(t *testing.T, rec *e2eResolutionRecorder, f *issue767Fixture, db *sql.DB) {
	t.Helper()
	f.write(filepath.Join(f.primary, "producer", "producer.go"), e2eResolutionProducerSource(e2eResolutionProducerWithdrawn))
	e2eResolutionAwaitNodeGone(f, db, e2eResolutionProduceID)
	f.settle()

	surviving := e2eResolutionRows(t, db,
		"SELECT to_id||' origin='||origin||' tier='||tier||' conf='||CAST(confidence AS TEXT) FROM edges WHERE view_gen=0 AND from_id=? AND kind='calls'", e2eResolutionConsumeID)
	var dishonest, rebound []string
	for _, row := range surviving {
		target, _, _ := strings.Cut(row, " origin=")
		unresolved := strings.Contains(target, "unresolved::") || strings.Contains(target, "external::")
		if unresolved && !strings.Contains(row, "origin= tier= conf=0") {
			dishonest = append(dishonest, row)
		}
		if strings.Contains(target, "/decoy/") || strings.Contains(target, "/vendor/") || strings.HasPrefix(target, e2eResolutionOtherRepoName) {
			rebound = append(rebound, row)
		}
		if target == e2eResolutionProduceID {
			dishonest = append(dishonest, row+" (still claims the withdrawn definition)")
		}
	}
	rec.assert("unresolved_facts_degrade_truthfully", len(dishonest) == 0 && len(rebound) == 0,
		"outgoing call edges after the definition was withdrawn: %v; advertising a tier they no longer earn: %v; falsely rebound to a same-named symbol: %v",
		surviving, dishonest, rebound)
}

// Stage 5 — the definition comes back, byte-identical to stage 3.
func e2eResolutionStageRestore(t *testing.T, rec *e2eResolutionRecorder, f *issue767Fixture, db *sql.DB, want string) {
	t.Helper()
	f.write(filepath.Join(f.primary, "producer", "producer.go"), e2eResolutionProducerSource(e2eResolutionProducerRestored))
	e2eResolutionAwaitNodeLine(f, db, e2eResolutionProduceID, e2eResolutionProduceLine(e2eResolutionProducerRestored))
	f.settle()
	got := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	outgoing := e2eResolutionRows(t, db, "SELECT to_id||' origin='||origin||' tier='||tier FROM edges WHERE view_gen=0 AND from_id=? AND kind='calls'", e2eResolutionConsumeID)
	rec.assert("rebind_restores_exact_identity", got == want && got != "",
		"incoming edge after restoring the definition %q; before the withdrawal %q; every outgoing call the consumer now holds: %v", got, want, outgoing)
}

// Stage 6 — the second repository. It runs on the COLD state, before any
// mutation: whether the primary's resolution is deflected by a second
// repository declaring the same module path is a property of the corpus, and
// running it after the provenance stages would let an unrelated defect there
// masquerade as a cross-repository one.
func e2eResolutionStageCrossRepository(t *testing.T, rec *e2eResolutionRecorder, f *issue767Fixture, db *sql.DB) {
	t.Helper()
	outward := e2eResolutionRows(t, db, "SELECT from_id||' -'||kind||'-> '||to_id||' cross='||CAST(cross_repo AS TEXT) FROM edges WHERE view_gen=0 AND from_id LIKE ? AND to_id LIKE ?",
		issue767FixturePrefix+"/%", e2eResolutionOtherRepoName+"/%")
	inward := e2eResolutionRows(t, db, "SELECT from_id||' -'||kind||'-> '||to_id||' cross='||CAST(cross_repo AS TEXT) FROM edges WHERE view_gen=0 AND from_id LIKE ? AND to_id LIKE ?",
		e2eResolutionOtherRepoName+"/%", issue767FixturePrefix+"/%")
	otherIndexed := e2eResolutionRows(t, db, "SELECT id FROM nodes WHERE view_gen=0 AND repo_prefix=?", e2eResolutionOtherRepoName)
	if len(otherIndexed) == 0 {
		rec.skip("cross_repository_producer_control",
			"the second repository produced no nodes in this run, so a cross-repository producer control cannot be exercised")
		return
	}
	bind := e2eResolutionEdgeRow(t, db, e2eResolutionConsumeID, e2eResolutionProduceID, "calls")
	// A deflection is a resolver-bound REFERENCE out of the primary whose
	// target lives in the second repository — the primary resolving its own
	// import to a producer it does not name. An outward value_flow from a
	// primary symbol into the other repository's consumer is the second
	// repository consuming the primary, seen from the other end, and is
	// recorded rather than asserted.
	deflected := []string{}
	for _, row := range outward {
		_, target, found := strings.Cut(row, "-> ")
		if !found || !strings.HasPrefix(target, e2eResolutionOtherRepoName+"/") {
			continue
		}
		if e2eResolutionIsResolvedReference(row) {
			deflected = append(deflected, row)
		}
	}
	rec.assert("cross_repository_producer_control", len(deflected) == 0 && bind != "",
		"the second repository (same module path, same package, same symbol) contributed %d nodes; the primary's own binding is %q; edges out of the primary into it: %v (deflected producer binds: %v); edges INTO the primary from it, recorded not asserted: %v",
		len(otherIndexed), bind, outward, deflected, inward)
}

// e2eResolutionDynamicEdgeRows reads the resolver-bound reference edges out of one
// symbol, rendered as "from -kind-> to origin=… tier=…".
func e2eResolutionDynamicEdgeRows(t *testing.T, db *sql.DB, from string) []string {
	t.Helper()
	return e2eResolutionRows(t, db,
		"SELECT from_id||' -'||kind||'-> '||to_id||' origin='||origin||' tier='||tier "+
			"FROM edges WHERE view_gen=0 AND from_id=? AND kind IN ("+e2eResolutionResolvedEdgeKinds+")", from)
}

// e2eResolutionDynamicEdgeParts splits a rendered edge row into its edge kind, its
// target and its provenance suffix.
func e2eResolutionDynamicEdgeParts(row string) (kind, target, provenance string) {
	head, provenance, found := strings.Cut(row, " origin=")
	if !found {
		head, provenance = row, ""
	} else {
		provenance = "origin=" + provenance
	}
	_, rest, found := strings.Cut(head, " -")
	if !found {
		return "", head, provenance
	}
	kind, target, found = strings.Cut(rest, "-> ")
	if !found {
		return "", rest, provenance
	}
	return kind, target, provenance
}

// e2eResolutionDynamicGuessVerdict applies the matrix's uniform standard for a
// dynamically reached bind.
//
// A guess is a bind out of the dynamic entry point that lands on a concrete
// repository symbol: the runtime handle names nothing at index time, so the
// binder cannot have proved it. Unresolved, external and stdlib terminals are
// NOT guesses — they are the honest shapes a conservative binder is supposed to
// emit, and this file treats them that way everywhere.
//
// A guess is allowed to exist; what it may not do is look exactly like a proof.
// The static sibling resolves the same target through an ordinary import, so a
// guess carrying the same edge kind, the same target and the same provenance as
// that proof leaves the graph unable to tell them apart — which is the failure.
func e2eResolutionDynamicGuessVerdict(dynamic, static []string) (guesses, indistinguishable []string) {
	proofs := map[string]bool{}
	for _, row := range static {
		kind, target, provenance := e2eResolutionDynamicEdgeParts(row)
		proofs[kind+"\x00"+target+"\x00"+provenance] = true
	}
	for _, row := range dynamic {
		kind, target, provenance := e2eResolutionDynamicEdgeParts(row)
		if strings.Contains(target, "unresolved::") || strings.Contains(target, "external::") || strings.Contains(target, "stdlib") {
			continue
		}
		guesses = append(guesses, row)
		if proofs[kind+"\x00"+target+"\x00"+provenance] {
			indistinguishable = append(indistinguishable, row)
		}
	}
	sort.Strings(guesses)
	sort.Strings(indistinguishable)
	return guesses, indistinguishable
}

// e2eResolutionIsResolvedReference reports whether a rendered edge row is one of the
// resolver-bound reference kinds — the kinds a manifest's import graph decides,
// as opposed to the derived kinds a later pass synthesises.
func e2eResolutionIsResolvedReference(row string) bool {
	for _, kind := range strings.Split(strings.ReplaceAll(e2eResolutionResolvedEdgeKinds, "'", ""), ",") {
		if strings.Contains(row, " -"+kind+"-> ") {
			return true
		}
	}
	return false
}

// Stage 7 — provider mutation evidence (H8).
func e2eResolutionStageProviderEvidence(t *testing.T, rec *e2eResolutionRecorder, f *issue767Fixture, db *sql.DB) {
	t.Helper()
	providers := e2eResolutionRows(t, db, "SELECT provider||' coverage='||CAST(coverage AS TEXT) FROM enrichment_state WHERE view_gen=0 AND repo_prefix=?", issue767FixturePrefix)
	real := []string{}
	for _, row := range providers {
		if !strings.HasPrefix(row, "__repo__") {
			real = append(real, row)
		}
	}
	if len(real) == 0 {
		rec.skip("provider_mutation_evidence",
			fmt.Sprintf("no enrichment provider ran in this isolated build (recorded rows: %v), so there is no provider mutation to evidence", providers))
		return
	}
	rec.observe("provider_mutation_evidence", "enrichment providers recorded after the mutation sequence: %v", real)
}

// Stage 8 — the gate-1 oracle: a fresh isolated index of the same tree.
func e2eResolutionStageOracle(t *testing.T, rec *e2eResolutionRecorder, arm *issue767Fixture, armDB *sql.DB, binary, artifactDir string) {
	t.Helper()
	tree := e2eResolutionCopyTree(t, arm.primary)
	oracle := e2eResolutionSetup(t, binary, tree)
	t.Logf("resolution matrix fresh oracle root: %s", oracle.root)
	oracle.start()
	oracleDB := oracle.openReadOnly()
	oracle.awaitSymbolIn(oracle.primary, "Issue767PrimaryMarker", filepath.Join(oracle.primary, "marker.go"), 3*time.Minute)
	oracle.settle()

	outputs := e2eResolutionOutputs()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	incremental, incrementalNotes := e2eResolutionReadProjections(ctx, armDB, outputs)
	fresh, freshNotes := e2eResolutionReadProjections(ctx, oracleDB, outputs)
	// Same rule on the oracle: a projection that could not be READ writes zero
	// rows, and zero rows on both arms is reported as "identical, exercised
	// nothing". The two are not the same thing, and only one of them is a
	// measurement.
	for _, problem := range e2eResolutionUnreadableProjections(map[string]map[string]string{"incremental arm": incrementalNotes, "fresh oracle": freshNotes}) {
		t.Errorf("resolution matrix oracle: %s; an unreadable projection must not be mistaken for an empty one", problem)
	}

	e2eResolutionWriteProjections(t, filepath.Join(artifactDir, "projections-incremental.txt"), outputs, incremental)
	e2eResolutionWriteProjections(t, filepath.Join(artifactDir, "projections-fresh.txt"), outputs, fresh)

	summary := e2eResolutionSummariseDerived(fresh, incremental, outputs)

	divergences := e2eResolutionCompare(fresh, incremental, outputs)
	classification := e2eResolutionClassify(divergences, e2eResolutionKnownGaps())
	for _, divergence := range divergences {
		t.Logf("resolution matrix divergence in %s: missing=%v extra=%v", divergence.Output, divergence.Missing, divergence.Extra)
	}

	var explainedGaps []string
	for id, rows := range classification.Matched {
		explainedGaps = append(explainedGaps, fmt.Sprintf("%s (%d rows)", id, len(rows)))
	}
	sort.Strings(explainedGaps)

	switch {
	case len(classification.Unmatched) > 0:
		rec.record("context_separated_build_matches_fresh_index", e2eResolutionFail,
			"the incrementally reached view disagrees with a fresh isolated index of the same tree in ways no declared gap explains: %v; declared gaps that also reproduced: %v; %s",
			classification.Unmatched, explainedGaps, summary.Describe())
	case len(classification.Stale) > 0:
		rec.record("context_separated_build_matches_fresh_index", e2eResolutionFail,
			"declared gaps %v no longer reproduce — delete them from e2eResolutionKnownGaps and close the ledger row rather than carrying a stale allowlist; %s",
			classification.Stale, summary.Describe())
	case len(divergences) > 0:
		rec.record("context_separated_build_matches_fresh_index", e2eResolutionGap,
			"every divergence is a declared gap: %v; %s",
			explainedGaps, summary.Describe())
	default:
		rec.record("context_separated_build_matches_fresh_index", e2eResolutionPass,
			"no divergence from a fresh isolated index of the same tree; %s", summary.Describe())
	}
}

func e2eResolutionWriteProjections(t *testing.T, path string, outputs []e2eResolutionOutput, rows map[string][]string) {
	t.Helper()
	var b strings.Builder
	for _, output := range outputs {
		fmt.Fprintf(&b, "### %s (derived=%t)\n%s\n", output.ID, output.Derived, output.Why)
		for _, row := range rows[output.ID] {
			b.WriteString("  " + row + "\n")
		}
		b.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Errorf("resolution matrix: writing %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// Offline tests. These run in every ordinary `go test` of this package: they
// pin the matrix's own logic without a daemon, a binary or a network.
// ---------------------------------------------------------------------------

func TestE2EMatrixResolutionEveryCaseHasItsCorpus(t *testing.T) {
	corpus := map[string]bool{}
	for _, file := range e2eResolutionCorpusFiles() {
		if corpus[file.Path] {
			t.Errorf("corpus declares %s twice", file.Path)
		}
		corpus[file.Path] = true
	}
	seen := map[string]bool{}
	for _, c := range e2eResolutionCases() {
		if seen[c.ID] {
			t.Errorf("case %s is declared twice", c.ID)
		}
		seen[c.ID] = true
		if c.Gate == "" {
			t.Errorf("case %s names no acceptance gate", c.ID)
		}
		if strings.TrimSpace(c.What) == "" {
			t.Errorf("case %s says nothing about what it asserts", c.ID)
		}
		if len(c.Needs) == 0 {
			t.Errorf("case %s needs no corpus file; every case has to read something", c.ID)
		}
		for _, need := range c.Needs {
			if !corpus[need] {
				t.Errorf("case %s needs corpus file %s, which the corpus does not declare", c.ID, need)
			}
		}
	}
	// The matrix bullet's manifest shapes each have a file.
	for _, required := range []string{"decoy/go.mod", "vendor/modules.txt", "broken/go.mod", "oversized/go.mod", "unreadable/go.mod"} {
		if !corpus[required] {
			t.Errorf("the matrix bullet names a manifest shape with no corpus file: %s", required)
		}
	}
	// A case that names a manifest must also name a file that manifest's own
	// directory holds, or its stated subject is not in the corpus at all and
	// the assertion can only observe unrelated directories. The one exemption
	// is a case declared unexercisable in source, which asserts nothing.
	for _, c := range e2eResolutionCases() {
		if c.Unexercisable != "" {
			continue
		}
		for _, need := range c.Needs {
			dir, base := path.Split(need)
			if base != "go.mod" && base != "go.work" && base != "modules.txt" && base != "package.json" {
				continue
			}
			// A ROOT manifest's directory is the repository, which every other
			// corpus file is under; the vacuity this rule catches is a NESTED
			// manifest with no source file of its own.
			if dir == "" {
				continue
			}
			sibling := false
			for _, other := range c.Needs {
				if other == need || !strings.HasPrefix(other, dir) {
					continue
				}
				sibling = true
			}
			if !sibling {
				t.Errorf("case %s names manifest %s but no file under %s: its stated subject is not in the corpus, so the assertion could only observe unrelated directories",
					c.ID, need, dir)
			}
		}
	}
	// Every declared gap's causal claim has a case that measures it.
	if !seen["declared_gaps_are_caused_by_the_reparse_alone"] {
		t.Error("no case measures the cause the declared gaps attribute their rows to; a causal claim with no measurement is prose")
	}
	// And the H8 hazards each have a case.
	hazards := map[string]bool{}
	for _, c := range e2eResolutionCases() {
		if c.Hazard != "" {
			hazards[c.Hazard] = true
		}
	}
	for _, hazard := range []string{"H8 module-root mismatch", "H8 manifest-only invalidation", "H8 mixed-language imports", "H8 provider mutation evidence", "H8 dynamic placement"} {
		if !hazards[hazard] {
			t.Errorf("resolution hazard %q has no case", hazard)
		}
	}
}

func TestE2EMatrixResolutionCorpusIsDeterministicAndOversizedManifestExceedsTheCap(t *testing.T) {
	first, second := e2eResolutionCorpusFiles(), e2eResolutionCorpusFiles()
	if len(first) != len(second) {
		t.Fatalf("corpus length is not stable: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("corpus entry %d is not stable: %+v vs %+v", i, first[i], second[i])
		}
	}
	var oversized, unreadable *e2eResolutionSourceFile
	for i, file := range first {
		switch file.Path {
		case "oversized/go.mod":
			oversized = &first[i]
		case "unreadable/go.mod":
			unreadable = &first[i]
		}
	}
	if oversized == nil || unreadable == nil {
		t.Fatal("the corpus lost the oversized or the unreadable manifest")
	}
	if len(oversized.Content) <= e2eResolutionMaxFileSize {
		t.Errorf("the oversized manifest is %d bytes, which does not exceed the configured cap of %d", len(oversized.Content), e2eResolutionMaxFileSize)
	}
	if !unreadable.Unreadable {
		t.Error("the unreadable manifest is not marked unreadable, so the fixture would never chmod it")
	}
	if oversized.Unreadable {
		t.Error("the oversized manifest must be readable; its size is the gate under test, not its mode")
	}

	// The arm that can actually fail: an oversized SOURCE file and the in-spec
	// control beside it. Both must be Go (so both reach the cap at
	// internal/indexer/walk_source.go:88), the big one over the cap, the
	// control under it, and the big one must declare its symbol BEFORE the
	// padding so an indexer that ignored the cap would certainly find it.
	var big, control *e2eResolutionSourceFile
	for i, file := range first {
		switch file.Path {
		case "bigsource/big.go":
			big = &first[i]
		case "bigsource/small.go":
			control = &first[i]
		}
	}
	if big == nil || control == nil {
		t.Fatal("the corpus lost the oversized source file or its in-spec control")
	}
	if len(big.Content) <= e2eResolutionMaxFileSize {
		t.Errorf("the oversized source file is %d bytes, which does not exceed the configured cap of %d", len(big.Content), e2eResolutionMaxFileSize)
	}
	if len(control.Content) >= e2eResolutionMaxFileSize {
		t.Errorf("the control file is %d bytes, which does not stay under the cap of %d — then both would be skipped and the case would be vacuous", len(control.Content), e2eResolutionMaxFileSize)
	}
	if big.Unreadable || control.Unreadable {
		t.Error("the oversized-source arm must be readable; size is the gate under test, not mode")
	}
	declaration := "func " + e2eResolutionBigSourceSymName + "("
	index := strings.Index(big.Content, declaration)
	if index < 0 {
		t.Fatalf("the oversized source file does not declare %s, so an indexer ignoring the cap could not produce it either and the assertion could not fail", e2eResolutionBigSourceSymName)
	}
	if index > e2eResolutionMaxFileSize {
		t.Errorf("%s is declared at byte %d, past the cap — a partial reader could miss it for a reason that is not the cap", e2eResolutionBigSourceSymName, index)
	}
	if !strings.Contains(control.Content, "func GxResolutionOversizedControlValue(") {
		t.Error("the control file declares no symbol, so it cannot disprove 'nothing in this directory is indexed'")
	}
	if !strings.HasPrefix(big.Content, "package bigsource\n") || !strings.HasPrefix(control.Content, "package bigsource\n") {
		t.Error("the oversized file and its control must be the same package, or the control controls for nothing")
	}

	// The sibling beside the unparseable manifest: without it "an unparseable
	// manifest does not stop its siblings being indexed" has no sibling.
	var sibling *e2eResolutionSourceFile
	for i, file := range first {
		if file.Path == "broken/broken.go" {
			sibling = &first[i]
		}
	}
	if sibling == nil {
		t.Fatal("the corpus declares no source file beside the unparseable manifest")
	}
	if !strings.Contains(sibling.Content, "func GxResolutionBrokenSiblingValue(") {
		t.Error("the sibling beside the unparseable manifest declares no symbol to observe")
	}
	if e2eResolutionCorpusFileSize("broken/go.mod") <= 0 {
		t.Error("the unparseable manifest is empty; an empty file is not an unparseable one")
	}
	if got := e2eResolutionCorpusFileSize("no/such/file"); got != -1 {
		t.Errorf("e2eResolutionCorpusFileSize invented a size for an undeclared path: %d", got)
	}
}

func TestE2EMatrixResolutionOversizedManifestArmIsDeclaredUnexercisableRatherThanPassed(t *testing.T) {
	var oversized *e2eResolutionCase
	cases := e2eResolutionCases()
	for i, c := range cases {
		if c.ID == "oversized_manifest" {
			oversized = &cases[i]
		}
	}
	if oversized == nil {
		t.Fatal("the oversized-manifest case is gone")
	}
	if oversized.Unexercisable == "" {
		t.Fatal("index.max_file_size is never consulted on a manifest path, so the oversized-manifest case has to be declared unexercisable rather than assert a result it cannot produce")
	}
	// The declaration has to name the production reasons, not just hand-wave.
	for _, want := range []string{"walk_source.go", "incremental_contracts.go", "size_skip_census.go"} {
		if !strings.Contains(oversized.Unexercisable, want) {
			t.Errorf("the unexercisable declaration does not cite %s", want)
		}
	}
	// And a pass recorded for it must be rejected by the validator, which is
	// the whole point of declaring it in source.
	rows := []e2eResolutionOutcome{}
	for _, c := range cases {
		status, detail := e2eResolutionPass, "ok"
		if c.Unexercisable != "" {
			status, detail = e2eResolutionNotExercised, c.Unexercisable+" (ledger row "+e2eResolutionLedgerRow+")"
		}
		rows = append(rows, e2eResolutionOutcome{Case: c.ID, Gate: c.Gate, Status: status, Detail: detail})
	}
	if problems := e2eResolutionValidateOutcomes(cases, rows); len(problems) != 0 {
		t.Fatalf("a table honouring every declaration was rejected: %v", problems)
	}
	for i := range rows {
		if rows[i].Case != "oversized_manifest" {
			continue
		}
		rows[i].Status, rows[i].Detail = e2eResolutionPass, "nodes from the 2097152-byte manifest: []"
	}
	problems := e2eResolutionValidateOutcomes(cases, rows)
	if len(problems) == 0 {
		t.Fatal("a pass recorded for a case declared unexercisable was accepted — exactly the silent pass this declaration exists to stop")
	}
	joined := strings.Join(problems, " | ")
	if !strings.Contains(joined, "cannot fail") {
		t.Errorf("the validator did not say why the pass is refused: %v", problems)
	}

	// The second half of the same guard: the right STATUS with a hand-waved
	// reason is not the declaration either. Without this the row could say
	// "skipped, see the report" and the production citations that make the
	// declaration checkable would never have to appear in the outcome table.
	for i := range rows {
		if rows[i].Case != "oversized_manifest" {
			continue
		}
		rows[i].Status = e2eResolutionNotExercised
		rows[i].Detail = "not exercised this run (ledger row " + e2eResolutionLedgerRow + ")"
	}
	problems = e2eResolutionValidateOutcomes(cases, rows)
	if len(problems) == 0 {
		t.Fatal("a not_exercised row that does not carry the declared reason was accepted; the declaration's citations are what make it checkable")
	}
	if joined := strings.Join(problems, " | "); !strings.Contains(joined, "declared reason") {
		t.Errorf("the validator did not say the declared reason is missing: %v", problems)
	}
}

func TestE2EMatrixResolutionEveryCaseSaysHowItCouldFail(t *testing.T) {
	cases := e2eResolutionCases()
	for _, c := range cases {
		switch {
		case c.Falsifier == "" && c.Unexercisable == "":
			t.Errorf("case %s says neither what observable would make it fail nor why no such observable exists; that is how an assertion that cannot fail gets recorded as a pass", c.ID)
		case c.Falsifier != "" && c.Unexercisable != "":
			t.Errorf("case %s declares both a falsifying observable and an unexercisable reason", c.ID)
		}
		if c.Falsifier == "" {
			continue
		}
		if len(c.Falsifier) < 40 {
			t.Errorf("case %s: %q is not a concrete observable", c.ID, c.Falsifier)
		}
		if c.Falsifier == c.What {
			t.Errorf("case %s restates What as its falsifier; the falsifier is the ROW that would make the assertion false, not the assertion", c.ID)
		}
	}

	// And the rule is enforced at run time, not only here: a case with neither
	// declaration fails the outcome table, and one that CAN fail may not be
	// recorded not_exercised without saying it asserted nothing.
	silent := []e2eResolutionCase{{ID: "one", Gate: "G1", Needs: []string{"go.mod"}, What: "x"}}
	rows := []e2eResolutionOutcome{{Case: "one", Status: e2eResolutionPass, Detail: "ok"}}
	problems := e2eResolutionValidateOutcomes(silent, rows)
	if len(problems) != 1 || !strings.Contains(problems[0], "neither a falsifying observable") {
		t.Fatalf("a case declaring neither was accepted: %v", problems)
	}
	both := []e2eResolutionCase{{ID: "one", Gate: "G1", Needs: []string{"go.mod"}, What: "x", Falsifier: "a row that would have to appear for this to be false", Unexercisable: "and also it cannot happen"}}
	if problems := e2eResolutionValidateOutcomes(both, rows); !strings.Contains(strings.Join(problems, " | "), "one or the other") {
		t.Fatalf("a case declaring both was accepted: %v", problems)
	}
	falsifiable := []e2eResolutionCase{{ID: "one", Gate: "G1", Needs: []string{"go.mod"}, What: "x", Falsifier: "a row that would have to appear for this to be false"}}
	quiet := []e2eResolutionOutcome{{Case: "one", Status: e2eResolutionNotExercised, Detail: "skipped this run (ledger row " + e2eResolutionLedgerRow + ")"}}
	if problems := e2eResolutionValidateOutcomes(falsifiable, quiet); len(problems) != 1 || !strings.Contains(problems[0], e2eResolutionNothingAsserted) {
		t.Fatalf("a case that can fail was skipped without saying it asserted nothing: %v", problems)
	}
	honest := []e2eResolutionOutcome{{Case: "one", Status: e2eResolutionNotExercised, Detail: "the control was absent, so " + e2eResolutionNothingAsserted + " (ledger row " + e2eResolutionLedgerRow + ")"}}
	if problems := e2eResolutionValidateOutcomes(falsifiable, honest); len(problems) != 0 {
		t.Fatalf("an honest measured skip was rejected: %v", problems)
	}
}

func TestE2EMatrixResolutionUnreadableProjectionIsNotAnEmptyOne(t *testing.T) {
	none := e2eResolutionUnreadableProjections(map[string]map[string]string{"incremental arm": {}, "fresh oracle": nil})
	if len(none) != 0 {
		t.Fatalf("a run in which every projection was readable reported %v", none)
	}
	// The real failure this guard exists for: a UNION arm naming a column its
	// table does not have. The projection returns no rows, and "no rows on both
	// arms" is reported as "exercised nothing" — a measurement the run never
	// made.
	problems := e2eResolutionUnreadableProjections(map[string]map[string]string{
		"incremental arm": {"nonserved_generation_markers": "SQL logic error: no such column: node_id (1)"},
		"fresh oracle":    {"nonserved_generation_markers": "SQL logic error: no such column: node_id (1)"},
	})
	if len(problems) != 2 {
		t.Fatalf("an unreadable projection on both arms reported %v", problems)
	}
	for _, problem := range problems {
		if !strings.Contains(problem, "nonserved_generation_markers") || !strings.Contains(problem, "no such column") {
			t.Errorf("the report does not name the projection and the reason: %s", problem)
		}
	}
	if !strings.HasPrefix(problems[0], "fresh oracle") || !strings.HasPrefix(problems[1], "incremental arm") {
		t.Errorf("the arms are not named or not ordered stably: %v", problems)
	}
	// And every arm of the recorded projection has to name only columns the
	// schema declares. The one that did not is pinned by name so the mistake
	// cannot come back unnoticed.
	for _, arm := range e2eResolutionNonServedArms() {
		if arm.Table != "content_fts_rowid" {
			continue
		}
		if strings.Contains(arm.Select, "node_id") {
			t.Error("content_fts_rowid has no node_id column (internal/graph/store_sqlite/schema.go:1486-1500); selecting one makes the whole UNION unreadable and the projection silently empty")
		}
		if !strings.Contains(arm.Select, "fts_rowid") || !strings.Contains(arm.Select, "file_path") {
			t.Errorf("the content_fts_rowid arm does not read the columns that table actually has: %s", arm.Select)
		}
	}
}

func TestE2EMatrixResolutionMeasuredSkipRecordsAnHonestNotExercisedRow(t *testing.T) {
	cases := e2eResolutionCases()
	rec := e2eResolutionNewRecorder(t)
	const measured = "the vendored tree contributed no node to the served generation ([])"
	for _, c := range cases {
		switch {
		case c.ID == "vendor_manifest":
			// The corpus-contingent skip: the observable this case needs was
			// not produced, so the run says so rather than passing.
			rec.skipMeasured(c.ID, measured)
		case c.ID == "provider_mutation_evidence":
			// The host-contingent skip: nothing about the corpus, but still
			// not a non-failure to be read as a result.
			rec.skip(c.ID, "no enrichment provider ran in this isolated build")
		case c.Unexercisable != "":
			rec.skipDeclared(c.ID, "the measured control")
		default:
			rec.assert(c.ID, true, "ok")
		}
	}
	if problems := e2eResolutionValidateOutcomes(cases, rec.rows); len(problems) != 0 {
		t.Fatalf("the run's own skip paths produce a table the validator rejects: %v", problems)
	}
	byCase := map[string]e2eResolutionOutcome{}
	for _, row := range rec.rows {
		byCase[row.Case] = row
	}
	vendor := byCase["vendor_manifest"]
	if vendor.Status != e2eResolutionNotExercised {
		t.Fatalf("a measured skip recorded %q", vendor.Status)
	}
	for _, want := range []string{e2eResolutionNothingAsserted, measured, e2eResolutionLedgerRow} {
		if !strings.Contains(vendor.Detail, want) {
			t.Errorf("the measured skip row does not carry %q: %s", want, vendor.Detail)
		}
	}
	// The row has to carry the case's own falsifier, so a reader sees what the
	// run would have had to observe — not merely that it observed nothing.
	for _, c := range cases {
		if c.ID != "vendor_manifest" {
			continue
		}
		if !strings.Contains(vendor.Detail, c.Falsifier) {
			t.Errorf("the measured skip row does not say what the case needed: %s", vendor.Detail)
		}
	}
	if provider := byCase["provider_mutation_evidence"]; !strings.Contains(provider.Detail, e2eResolutionNothingAsserted) {
		t.Errorf("a host-contingent skip does not say it asserted nothing: %s", provider.Detail)
	}
}

func TestE2EMatrixResolutionMixedVerdictJudgesTheTargetAndNotTheWholeRow(t *testing.T) {
	const want = e2eResolutionMixedNamedBind
	// The four maximally-wrong rows: the TypeScript entry point bound to the
	// same-named decoy in the other language, to the second repository, to the
	// Go producer and to the Ruby file. The previous shape rendered
	// "from -kind-> to" and filtered the WHOLE row for "/mixed/", which the
	// query's own `from_id LIKE 'issue767/mixed/%'` already guaranteed — so all
	// four passed. Each has to be judged on its TARGET.
	wrong := []string{
		e2eResolutionMixedEntryID + " -calls-> " + e2eResolutionMixedDecoyID,
		e2eResolutionMixedEntryID + " -calls-> " + e2eResolutionOtherRepoName + "/producer/producer.go::Produce",
		e2eResolutionMixedEntryID + " -calls-> " + e2eResolutionProduceID,
		e2eResolutionMixedEntryID + " -calls-> " + e2eResolutionRubyID,
	}
	named, decoyBinds := e2eResolutionMixedVerdict(wrong, want, e2eResolutionMixedDecoyPfx)
	if named {
		t.Error("the named cross-language bind was reported present although no row carries it")
	}
	if len(decoyBinds) != 1 || !strings.Contains(decoyBinds[0], e2eResolutionMixedDecoyID) {
		t.Errorf("the bind onto the same-named decoy in the other language was not reported: %v", decoyBinds)
	}

	// The shape the corpus actually produces: the import pair plus the call.
	right := []string{
		issue767FixturePrefix + "/mixed/app.ts -imports-> " + issue767FixturePrefix + "/mixed/helper.js",
		issue767FixturePrefix + "/mixed/app.ts -imports-> " + e2eResolutionMixedHelperID,
		want,
	}
	named, decoyBinds = e2eResolutionMixedVerdict(right, want, e2eResolutionMixedDecoyPfx)
	if !named || len(decoyBinds) != 0 {
		t.Errorf("the correct binding was judged wrong: named=%t decoyBinds=%v", named, decoyBinds)
	}

	// An empty result is not a pass: with no rows at all, the named bind is
	// absent and the case fails. This is the other half of the previous
	// shape's vacuity — it had no non-empty control either.
	if named, _ := e2eResolutionMixedVerdict(nil, want, e2eResolutionMixedDecoyPfx); named {
		t.Error("an empty edge set reported the named bind present")
	}

	// A near-miss target — the same file, a different symbol — is not the
	// named bind.
	if named, _ := e2eResolutionMixedVerdict([]string{e2eResolutionMixedEntryID + " -calls-> " + issue767FixturePrefix + "/mixed/helper.js::other"}, want, e2eResolutionMixedDecoyPfx); named {
		t.Error("a bind to a different symbol of the right file was accepted as the named bind")
	}
}

func TestE2EMatrixResolutionMixedLanguageCaseCarriesADecoyItCouldHaveBoundTo(t *testing.T) {
	const importerPath, targetPath = "mixed/app.ts", "mixed/helper.js"
	corpus := map[string]string{}
	decoyPath := ""
	for _, file := range e2eResolutionCorpusFiles() {
		corpus[file.Path] = file.Content
		if file.Path == importerPath || file.Path == targetPath {
			continue
		}
		if strings.Contains(file.Content, "function "+e2eResolutionMixedHelperName) {
			decoyPath = file.Path
		}
	}
	importer, importerOK := corpus[importerPath]
	target, targetOK := corpus[targetPath]
	if !importerOK || !targetOK {
		t.Fatal("the mixed-language corpus is missing the importer or the file it imports")
	}
	if decoyPath == "" {
		t.Fatal("no corpus file outside the imported pair exports the same name, so the case has nothing it could have bound to wrongly and its 'not bound' half is vacuous")
	}
	if !strings.HasPrefix(e2eResolutionMixedDecoyID, issue767FixturePrefix+"/"+decoyPath+"::") {
		t.Errorf("the decoy the case queries (%s) is not the decoy the corpus carries (%s)", e2eResolutionMixedDecoyID, decoyPath)
	}
	if !strings.Contains(importer, "./helper.js") {
		t.Error("the TypeScript entry point does not import the JavaScript file by name, so there is no cross-language bind to assert")
	}
	if !strings.Contains(target, "function "+e2eResolutionMixedHelperName) {
		t.Errorf("the imported file does not export %s, so the named bind could not exist", e2eResolutionMixedHelperName)
	}
	if strings.Contains(importer, decoyPath) {
		t.Error("the importer names the decoy, so a bind to it would be correct rather than a mis-bind")
	}
	importerDir, _ := path.Split(importerPath)
	if strings.HasPrefix(decoyPath, importerDir) {
		t.Errorf("the decoy %s sits under %s, the directory the case queries as the SOURCE of the edges, so its own edges would be judged as the importer's", decoyPath, importerDir)
	}
	// The decoy has to be in the OTHER language of the pair: that is the
	// hazard — a by-name bind that crosses the language boundary to the wrong
	// file — and a decoy in the same language would test something else.
	if path.Ext(decoyPath) == path.Ext(targetPath) {
		t.Errorf("the decoy %s and the imported file %s are in the same language; the mixed-language hazard is a bind that crosses the boundary", decoyPath, targetPath)
	}
	if path.Ext(decoyPath) != path.Ext(importerPath) {
		t.Errorf("the decoy %s is not in the importer's language, so a by-name bind to it would not be the cross-language confusion the case is about", decoyPath)
	}
	// And the case has to declare it, or the corpus carries a control the run
	// never looks at.
	for _, c := range e2eResolutionCases() {
		if c.ID != "mixed_language_imports" {
			continue
		}
		needs := strings.Join(c.Needs, " ")
		if !strings.Contains(needs, "mixeddecoy/helper.ts") {
			t.Errorf("the mixed-language case does not name its decoy: %v", c.Needs)
		}
		if !strings.Contains(c.Falsifier, e2eResolutionMixedDecoyID) || !strings.Contains(c.Falsifier, e2eResolutionMixedNamedBind) {
			t.Errorf("the mixed-language case's falsifier names neither the bind that must exist nor the decoy it must not be: %q", c.Falsifier)
		}
	}
}

func TestE2EMatrixResolutionSizeSkipProbeSeparatesAStubFromAnOrdinaryFile(t *testing.T) {
	if !e2eResolutionSizeSkipMarked([]string{e2eResolutionBigSourceFileID + " kind=file lang=go size_skip=1"}) {
		t.Error("a node carrying the size-skip telemetry was not recognised")
	}
	// The row run 7 produced for a FULLY PARSED 2 MiB file: same kind, same
	// language, no marker. This is why "a file node with no symbols" is not on
	// its own evidence of the cap.
	if e2eResolutionSizeSkipMarked([]string{e2eResolutionBigSourceFileID + " kind=file lang=go size_skip=0"}) {
		t.Error("an ordinary file node was reported as size-skipped")
	}
	if e2eResolutionSizeSkipMarked(nil) {
		t.Error("an absent node was reported as size-skipped")
	}
	// A marker anywhere but the probe's own column must not count.
	if e2eResolutionSizeSkipMarked([]string{"issue767/" + e2eResolutionSizeSkipMarker + ".go kind=file lang=go size_skip=0"}) {
		t.Error("a path containing the marker text was read as the probe's answer")
	}
	if !strings.Contains(e2eResolutionSizeSkipMarker, "size") {
		t.Errorf("the probed key %q is not the one sizeSkipNode writes", e2eResolutionSizeSkipMarker)
	}
}

func TestE2EMatrixResolutionNonServedMarkersCoverEveryComparedGenerationTable(t *testing.T) {
	outputs := e2eResolutionOutputs()
	var recordOnly *e2eResolutionOutput
	for i, output := range outputs {
		if output.RecordOnly {
			recordOnly = &outputs[i]
		}
	}
	if recordOnly == nil {
		t.Fatal("no recorded-not-compared projection: the rows the comparison stops at view_gen 0 would simply disappear")
	}
	armed := map[string]bool{}
	for _, arm := range e2eResolutionNonServedArms() {
		if armed[arm.Table] {
			t.Errorf("table %s has two arms", arm.Table)
		}
		armed[arm.Table] = true
		if !strings.Contains(arm.Select, "view_gen<>0") {
			t.Errorf("arm %s does not scope itself OUTSIDE the served generation: %s", arm.Table, arm.Select)
		}
		if !strings.Contains(arm.Select, " "+arm.Table+" ") {
			t.Errorf("arm %s does not read the table it claims: %s", arm.Table, arm.Select)
		}
		if !strings.Contains(recordOnly.Query, arm.Select) {
			t.Errorf("arm %s is declared but not in the recorded projection's query", arm.Table)
		}
	}
	exempt := map[string]bool{}
	for _, table := range e2eResolutionFTSVirtualTables() {
		exempt[table] = true
	}
	for _, output := range outputs {
		if output.RecordOnly {
			continue
		}
		for _, table := range e2eResolutionQueryTables(output.Query) {
			if exempt[table] || armed[table] {
				continue
			}
			t.Errorf("compared projection %s reads %s and scopes it to the served generation, but no arm of %s records that table's non-served rows — the narrowing would hide them",
				output.ID, table, recordOnly.ID)
		}
	}
	// The exemption is not a free pass: an exempt table must genuinely carry no
	// generation column, and its ownership sidecar must have an arm.
	for _, table := range e2eResolutionFTSVirtualTables() {
		if !armed[table+"_rowid"] {
			t.Errorf("%s is exempt because it carries no generation column, but its generation-keyed sidecar %s_rowid has no arm", table, table)
		}
	}
	if len(e2eResolutionQueryTables("SELECT x FROM alpha JOIN beta ON 1 UNION SELECT y FROM gamma")) != 3 {
		t.Errorf("the table extractor missed a FROM or a JOIN: %v", e2eResolutionQueryTables("SELECT x FROM alpha JOIN beta ON 1 UNION SELECT y FROM gamma"))
	}
}

func TestE2EMatrixResolutionProducerRevisionsMoveTheDeclarationWithoutChangingIt(t *testing.T) {
	cold := e2eResolutionProducerSource(e2eResolutionProducerCold)
	commented := e2eResolutionProducerSource(e2eResolutionProducerCommented)
	restored := e2eResolutionProducerSource(e2eResolutionProducerRestored)
	withdrawn := e2eResolutionProducerSource(e2eResolutionProducerWithdrawn)
	if commented != restored {
		t.Error("the restored revision has to be byte-identical to the commented one, or the restore stage is comparing two different trees")
	}
	if cold == commented {
		t.Error("the comment-only revision has to differ from the cold one, or no re-parse happens")
	}
	declaration := "func Produce() int { return 7 }"
	if !strings.Contains(cold, declaration) || !strings.Contains(commented, declaration) {
		t.Error("the comment-only revision must keep the declaration byte-identical")
	}
	if strings.Contains(withdrawn, "func Produce(") {
		t.Error("the withdrawn revision still declares Produce")
	}
	for rev, want := range map[e2eResolutionProducerRevision]int64{e2eResolutionProducerCold: 3, e2eResolutionProducerCommented: 5, e2eResolutionProducerRestored: 5, e2eResolutionProducerWithdrawn: 0} {
		if got := e2eResolutionProduceLine(rev); got != want {
			t.Errorf("revision %d: declared line %d, want %d", rev, got, want)
		}
		if want == 0 {
			continue
		}
		lines := strings.Split(e2eResolutionProducerSource(rev), "\n")
		if int(want) > len(lines) || !strings.HasPrefix(lines[want-1], "func Produce(") {
			t.Errorf("revision %d does not declare Produce on line %d; the await would hang", rev, want)
		}
	}
}

func TestE2EMatrixResolutionDerivedOutputsAreTheTenAndCarryTheirObservable(t *testing.T) {
	outputs := e2eResolutionOutputs()
	byID := map[string]e2eResolutionOutput{}
	var derived []string
	for _, output := range outputs {
		if _, clash := byID[output.ID]; clash {
			t.Errorf("projection %s is declared twice", output.ID)
		}
		byID[output.ID] = output
		if strings.TrimSpace(output.Query) == "" {
			t.Errorf("projection %s has no query", output.ID)
		}
		if strings.TrimSpace(output.Why) == "" {
			t.Errorf("projection %s does not say what it observes", output.ID)
		}
		if output.Derived {
			derived = append(derived, output.ID)
		}
	}
	want := e2eResolutionDerivedOutputIDs()
	sort.Strings(want)
	sort.Strings(derived)
	if strings.Join(want, ",") != strings.Join(derived, ",") {
		t.Errorf("the ten derived outputs are %v; the matrix compares %v", want, derived)
	}
	// No assertion in this file may be a node count: the gate-1 oracle compares
	// facts. COUNT( in a projection would smuggle one in.
	for _, output := range outputs {
		if strings.Contains(strings.ToUpper(output.Query), "COUNT(") {
			t.Errorf("projection %s counts rows; gate 1 compares facts, not counts", output.ID)
		}
		// Every arm of the query, not merely the query as a whole. A UNION whose
		// second arm forgot its generation scope reads rows a LATER generation
		// wrote, which the arms reach by different histories — the exact shape
		// that turns a history difference into a reported view divergence.
		for i, arm := range strings.Split(output.Query, "UNION") {
			if !strings.Contains(arm, "view_gen") {
				t.Errorf("projection %s: arm %d does not scope itself to a generation: %s", output.ID, i, strings.TrimSpace(arm))
			}
		}
		if output.RecordOnly && output.Derived {
			t.Errorf("projection %s is one of the ten derived outputs and must be compared, not merely recorded", output.ID)
		}
		if output.RecordOnly && !strings.Contains(output.Why, "uncompared") && !strings.Contains(output.Why, "asserted on by nothing") {
			t.Errorf("projection %s is recorded but not compared and does not say so in its Why", output.ID)
		}
	}
}

func TestE2EMatrixResolutionCompareReportsEachDerivedOutputLossSeparately(t *testing.T) {
	outputs := e2eResolutionOutputs()
	base := map[string][]string{}
	for _, output := range outputs {
		base[output.ID] = []string{output.ID + " row a", output.ID + " row b"}
	}
	same := map[string][]string{}
	for id, rows := range base {
		same[id] = append([]string(nil), rows...)
	}
	if got := e2eResolutionCompare(base, same, outputs); len(got) != 0 {
		t.Fatalf("identical projections diverged: %+v", got)
	}
	for _, target := range e2eResolutionDerivedOutputIDs() {
		incremental := map[string][]string{}
		for id, rows := range base {
			if id == target {
				incremental[id] = rows[:1]
				continue
			}
			incremental[id] = append([]string(nil), rows...)
		}
		got := e2eResolutionCompare(base, incremental, outputs)
		if len(got) != 1 || got[0].Output != target {
			t.Fatalf("dropping a row from %s reported %+v", target, got)
		}
		if len(got[0].Missing) != 1 || got[0].Missing[0] != target+" row b" {
			t.Fatalf("%s: missing rows %v", target, got[0].Missing)
		}
		if len(got[0].Extra) != 0 {
			t.Fatalf("%s: unexpected extra rows %v", target, got[0].Extra)
		}
	}
	// A row the incremental arm invents is a divergence too, in the other
	// direction: an oracle that only looked for losses would pass a view that
	// resolved something the fresh index does not.
	extra := map[string][]string{}
	for id, rows := range base {
		extra[id] = append(append([]string(nil), rows...), "invented")
	}
	got := e2eResolutionCompare(base, extra, outputs)
	compared := 0
	recordOnly := map[string]bool{}
	for _, output := range outputs {
		if output.RecordOnly {
			recordOnly[output.ID] = true
			continue
		}
		compared++
	}
	if compared == len(outputs) {
		t.Fatal("no projection is recorded-only, so this test no longer proves the recorded-only lane is excluded from the comparison")
	}
	if len(got) != compared {
		t.Fatalf("an invented row in every projection reported %d divergences, want %d (the recorded-only projections are not compared)", len(got), compared)
	}
	for _, divergence := range got {
		if recordOnly[divergence.Output] {
			t.Errorf("recorded-only projection %s was compared", divergence.Output)
		}
		if len(divergence.Extra) != 1 || divergence.Extra[0] != "invented" {
			t.Fatalf("%s: extra rows %v", divergence.Output, divergence.Extra)
		}
	}
}

func TestE2EMatrixResolutionNormalizeRowHidesOnlyTheObjectNames(t *testing.T) {
	sha := "3a726505b3b6dab71d642646abf2389c0f3121e3"
	row := "issue767 indexed_sha=" + sha + " dirty=0"
	if got := e2eResolutionNormalizeRow(row); got != "issue767 indexed_sha=<sha> dirty=0" {
		t.Errorf("normalised %q", got)
	}
	// A node identity is not an object name and must survive untouched.
	identity := "issue767/producer/producer.go::Produce @issue767/producer/producer.go:5"
	if got := e2eResolutionNormalizeRow(identity); got != identity {
		t.Errorf("normalisation damaged a node identity: %q", got)
	}
	// A recorded read failure carries the absolute path of the fixture that
	// recorded it. Two arms over the same tree have different roots, so the
	// root — and only the root — is normalised away.
	failure := "issue767/unreadable/unreadable.go err=open /private/tmp/gxh-e2eres/gx767-1333051735/repo/unreadable/unreadable.go: permission denied denied=1"
	want := "issue767/unreadable/unreadable.go err=open <fixture-root>/repo/unreadable/unreadable.go: permission denied denied=1"
	if got := e2eResolutionNormalizeRow(failure); got != want {
		t.Errorf("a recorded read failure normalised to %q, want %q", got, want)
	}
	// The repository-relative half of the same row must not be touched: it is
	// the fact under comparison.
	if got := e2eResolutionNormalizeRow("issue767/unreadable/unreadable.go denied=1"); got != "issue767/unreadable/unreadable.go denied=1" {
		t.Errorf("normalisation damaged a repository-relative path: %q", got)
	}
}

func TestE2EMatrixResolutionClassifyFailsOnAnUndeclaredDivergenceAndOnAStaleGap(t *testing.T) {
	gaps := e2eResolutionKnownGaps()
	divergences := []e2eResolutionDivergence{
		{Output: "incoming_edges_from_context_files", Missing: []string{"a -imports-> b @f:3"}},
		{Output: "capability_dataflow_framework_edges", Missing: []string{"a -value_flow-> b @f:5 origin=ast_resolved"}},
		{Output: "restub_provenance", Missing: []string{"a -value_flow-> b @f:5 origin=ast_resolved tier= conf=0.0 label="}},
	}
	classified := e2eResolutionClassify(divergences, gaps)
	if len(classified.Unmatched) != 0 {
		t.Errorf("the declared gaps did not explain their own rows: %v", classified.Unmatched)
	}
	if len(classified.Stale) != 0 {
		t.Errorf("gaps reported stale while reproducing: %v", classified.Stale)
	}

	// An undeclared loss must not be absorbed by a declared one.
	withUndeclared := append([]e2eResolutionDivergence(nil), divergences...)
	withUndeclared = append(withUndeclared, e2eResolutionDivergence{Output: "nodes_and_locations", Missing: []string{"issue767/x.go::Gone kind=function"}})
	classified = e2eResolutionClassify(withUndeclared, gaps)
	if len(classified.Unmatched) != 1 || !strings.Contains(classified.Unmatched[0], "issue767/x.go::Gone") {
		t.Errorf("an undeclared divergence was not reported: %v", classified.Unmatched)
	}

	// The same row in the other direction is not the declared gap either.
	flipped := []e2eResolutionDivergence{{Output: "incoming_edges_from_context_files", Extra: []string{"a -imports-> b @f:3"}}}
	classified = e2eResolutionClassify(flipped, gaps)
	if len(classified.Unmatched) != 1 {
		t.Errorf("a gap declared for one direction matched the other: %v", classified.Matched)
	}

	// And a gap that stops reproducing has to be reported, not enjoyed.
	classified = e2eResolutionClassify(nil, gaps)
	if len(classified.Stale) != len(gaps) {
		t.Errorf("stale gaps: %v, want all %d", classified.Stale, len(gaps))
	}
}

func TestE2EMatrixResolutionIsResolvedReferenceSeparatesTheManifestKindsFromTheDerivedOnes(t *testing.T) {
	for _, row := range []string{
		"a -calls-> b @f:1",
		"a -imports-> b @f:1",
		"a -method_call-> b @f:1",
	} {
		if !e2eResolutionIsResolvedReference(row) {
			t.Errorf("%q is a resolver-bound reference and was not recognised", row)
		}
	}
	for _, row := range []string{
		"a -value_flow-> b @f:1",
		"a -defines-> b @f:1",
		"a -cross_repo_calls-> b @f:1",
		"a calls b",
	} {
		if e2eResolutionIsResolvedReference(row) {
			t.Errorf("%q is not a resolver-bound reference kind and was counted as one", row)
		}
	}
}

func TestE2EMatrixResolutionKnownGapsAreDeclaredCompletely(t *testing.T) {
	outputs := map[string]bool{}
	for _, output := range e2eResolutionOutputs() {
		outputs[output.ID] = true
	}
	seen := map[string]bool{}
	for _, gap := range e2eResolutionKnownGaps() {
		if seen[gap.ID] {
			t.Errorf("gap %s is declared twice", gap.ID)
		}
		seen[gap.ID] = true
		if !outputs[gap.Output] {
			t.Errorf("gap %s names projection %s, which the matrix does not compare", gap.ID, gap.Output)
		}
		if gap.Direction != "missing" && gap.Direction != "extra" {
			t.Errorf("gap %s has direction %q", gap.ID, gap.Direction)
		}
		if strings.TrimSpace(gap.Match) == "" {
			t.Errorf("gap %s matches every row", gap.ID)
		}
		if strings.TrimSpace(gap.Why) == "" {
			t.Errorf("gap %s does not say why the branch does not handle it", gap.ID)
		}
		if gap.Ledger == "" {
			t.Errorf("gap %s names no ledger row", gap.ID)
		}
	}
}

func TestE2EMatrixResolutionGapProjectionsCoverEveryDeclaredGapsObservable(t *testing.T) {
	covered := map[string]bool{}
	for _, output := range e2eResolutionGapProjections() {
		if covered[output.ID] {
			t.Errorf("projection %s appears twice in the re-parse snapshot", output.ID)
		}
		covered[output.ID] = true
		if !output.Derived {
			t.Errorf("projection %s is snapshotted at the re-parse but is not one of the ten derived outputs", output.ID)
		}
	}
	for _, gap := range e2eResolutionKnownGaps() {
		if !covered[gap.Output] {
			t.Errorf("gap %s is declared against %s, which the re-parse snapshot does not read — its cause could never be measured", gap.ID, gap.Output)
		}
	}
	if len(covered) == 0 {
		t.Fatal("the re-parse snapshot reads nothing")
	}
}

func TestE2EMatrixResolutionUnattributedGapsNeedsTheReparseToHaveDroppedTheRows(t *testing.T) {
	gaps := e2eResolutionKnownGaps()
	full := e2eResolutionReparseDelta{Lost: map[string][]string{}, Gained: map[string][]string{}}
	for _, gap := range gaps {
		row := "issue767/a.go::A" + gap.Match + "issue767/b.go::B"
		if gap.Direction == "extra" {
			full.Gained[gap.Output] = append(full.Gained[gap.Output], row)
			continue
		}
		full.Lost[gap.Output] = append(full.Lost[gap.Output], row)
	}
	if unattributed := e2eResolutionUnattributedGaps(full, gaps); len(unattributed) != 0 {
		t.Fatalf("gaps whose rows the re-parse did drop were reported unattributed: %v", unattributed)
	}

	// A re-parse that dropped nothing supports no causal claim at all.
	none := e2eResolutionReparseDelta{Lost: map[string][]string{}, Gained: map[string][]string{}}
	if unattributed := e2eResolutionUnattributedGaps(none, gaps); len(unattributed) != len(gaps) {
		t.Fatalf("a re-parse that dropped nothing attributed %d of %d gaps: %v", len(gaps)-len(unattributed), len(gaps), unattributed)
	}

	// A loss in the WRONG projection does not attribute the gap either: that
	// is the substitution that would let the withdraw/restore sequence's own
	// damage be reported as the re-parse's.
	wrongOutput := e2eResolutionReparseDelta{Lost: map[string][]string{"nodes_and_locations": {"issue767/a.go::A" + gaps[0].Match + "issue767/b.go::B"}}, Gained: map[string][]string{}}
	if unattributed := e2eResolutionUnattributedGaps(wrongOutput, gaps); len(unattributed) != len(gaps) {
		t.Fatalf("a loss in an unrelated projection attributed a gap: %v", unattributed)
	}

	// And a loss in the right projection but of an unrelated row does not.
	wrongRow := e2eResolutionReparseDelta{Lost: map[string][]string{gaps[0].Output: {"issue767/a.go::A -defines-> issue767/a.go::B"}}, Gained: map[string][]string{}}
	unattributed := e2eResolutionUnattributedGaps(wrongRow, gaps)
	if len(unattributed) != len(gaps) {
		t.Fatalf("an unrelated row in the right projection attributed a gap: %v", unattributed)
	}
	if !strings.Contains(strings.Join(unattributed, " "), gaps[0].ID) {
		t.Errorf("the unattributed report does not name the gap: %v", unattributed)
	}

	// The direction matters: a gap declared "missing" is not attributed by a
	// row the re-parse GAINED.
	flipped := e2eResolutionReparseDelta{Lost: map[string][]string{}, Gained: map[string][]string{gaps[0].Output: {"issue767/a.go::A" + gaps[0].Match + "issue767/b.go::B"}}}
	if unattributed := e2eResolutionUnattributedGaps(flipped, gaps); len(unattributed) != len(gaps) {
		t.Fatalf("a gained row attributed a gap declared missing: %v", unattributed)
	}
}

func TestE2EMatrixResolutionReparseDeltaOfReportsBothDirectionsPerProjection(t *testing.T) {
	outputs := e2eResolutionGapProjections()
	if len(outputs) < 2 {
		t.Fatalf("expected the re-parse snapshot to read several projections, got %d", len(outputs))
	}
	before := map[string][]string{outputs[0].ID: {"kept", "lost"}, outputs[1].ID: {"kept"}}
	after := map[string][]string{outputs[0].ID: {"kept"}, outputs[1].ID: {"kept", "gained"}}
	delta := e2eResolutionReparseDeltaOf(before, after, outputs)
	if got := delta.Lost[outputs[0].ID]; len(got) != 1 || got[0] != "lost" {
		t.Errorf("lost rows for %s: %v", outputs[0].ID, got)
	}
	if got := delta.Gained[outputs[1].ID]; len(got) != 1 || got[0] != "gained" {
		t.Errorf("gained rows for %s: %v", outputs[1].ID, got)
	}
	if _, ok := delta.Gained[outputs[0].ID]; ok {
		t.Errorf("a projection that only lost rows reported a gain: %v", delta.Gained)
	}
	if _, ok := delta.Lost[outputs[1].ID]; ok {
		t.Errorf("a projection that only gained rows reported a loss: %v", delta.Lost)
	}
	unchanged := e2eResolutionReparseDeltaOf(before, before, outputs)
	if len(unchanged.Lost) != 0 || len(unchanged.Gained) != 0 {
		t.Errorf("an unchanged snapshot reported a delta: %+v", unchanged)
	}
}

func TestE2EMatrixResolutionReparseDeltaSeparatesAMovedRowFromALostOne(t *testing.T) {
	outputs := e2eResolutionGapProjections()
	id := outputs[0].ID
	const factA = "issue767/producer/producer.go::Produce -returns-> issue767::builtin::go::type::int"
	const factB = "issue767/producer/producer.go::Produce -value_flow-> issue767/consumer/consumer.go::Consume"
	const provenance = " origin=ast_resolved tier= conf=0.0 label="

	// factA moves from line 3 to line 5 — the comment-only revision doing
	// exactly what it was written to do. factB simply goes.
	before := map[string][]string{id: {factA + " @issue767/producer/producer.go:3" + provenance, factB + " @issue767/consumer/consumer.go:5" + provenance}}
	after := map[string][]string{id: {factA + " @issue767/producer/producer.go:5" + provenance}}
	delta := e2eResolutionReparseDeltaOf(before, after, outputs)
	if got := delta.Moved[id]; len(got) != 1 || !strings.Contains(got[0], "-returns->") {
		t.Errorf("the relocated row was not reported as moved: %v", delta.Moved)
	}
	if got := delta.Lost[id]; len(got) != 1 || !strings.Contains(got[0], "-value_flow->") {
		t.Errorf("lost rows: %v — the relocated row must not be counted as a loss and the vanished one must", got)
	}
	if len(delta.Gained[id]) != 0 {
		t.Errorf("the relocated row was also reported as a gain: %v", delta.Gained)
	}

	// A row that appears where nothing comparable left is a genuine gain.
	gainedDelta := e2eResolutionReparseDeltaOf(
		map[string][]string{id: {factA + " @issue767/producer/producer.go:3" + provenance}},
		map[string][]string{id: {factA + " @issue767/producer/producer.go:3" + provenance, factB + " @issue767/consumer/consumer.go:5" + provenance}},
		outputs)
	if got := gainedDelta.Gained[id]; len(got) != 1 || !strings.Contains(got[0], "-value_flow->") {
		t.Errorf("gained rows: %v", got)
	}
	if len(gainedDelta.Moved[id]) != 0 || len(gainedDelta.Lost[id]) != 0 {
		t.Errorf("an unchanged row was reported moved or lost: %+v", gainedDelta)
	}

	// A relocation that ALSO changes the provenance is not a move: the fact
	// itself changed, and this is the substitution that would let a real
	// provenance loss hide behind a line number.
	changed := e2eResolutionReparseDeltaOf(
		map[string][]string{id: {factA + " @issue767/producer/producer.go:3 origin=ast_resolved tier= conf=0.0 label="}},
		map[string][]string{id: {factA + " @issue767/producer/producer.go:5 origin= tier= conf=0.0 label="}},
		outputs)
	if len(changed.Moved[id]) != 0 {
		t.Errorf("a row whose provenance changed was excused as a move: %v", changed.Moved)
	}
	if len(changed.Lost[id]) != 1 || len(changed.Gained[id]) != 1 {
		t.Errorf("a provenance change was not reported as a loss plus a gain: %+v", changed)
	}
}

func TestE2EMatrixResolutionStripLocationRemovesOnlyTheLocation(t *testing.T) {
	row := "issue767/a.go::A -calls-> issue767/b.go::B @issue767/a.go:12 origin=ast_resolved tier= conf=0.0 label="
	want := "issue767/a.go::A -calls-> issue767/b.go::B origin=ast_resolved tier= conf=0.0 label="
	if got := e2eResolutionStripLocation(row); got != want {
		t.Errorf("stripped to %q, want %q", got, want)
	}
	// A node identity carries "::" and no " @path:line"; it must survive whole.
	identity := "issue767/a.go::A -calls-> issue767/b.go::B"
	if got := e2eResolutionStripLocation(identity); got != identity {
		t.Errorf("a row with no location was damaged: %q", got)
	}
}

func TestE2EMatrixResolutionDynamicGuessVerdictHoldsGuessAndProofToOneStandard(t *testing.T) {
	const dynamicTo = "issue767/dyn/app.py::run -calls-> issue767/dyn/helper.py::helper_value"
	const staticTo = "issue767/dyn/app.py::call_helper -calls-> issue767/dyn/helper.py::helper_value"

	// A bind inside /dyn/ is a guess, not an exemption: this is the case the
	// whole arm exists for, and the old shape waved it through.
	guesses, _ := e2eResolutionDynamicGuessVerdict([]string{dynamicTo + " origin=text_matched tier="}, nil)
	if len(guesses) != 1 {
		t.Fatalf("a bind reached through importlib to a concrete sibling symbol was not counted as a guess: %v", guesses)
	}

	// Same provenance as the static sibling's proof of the same target: the
	// graph cannot tell the guess from the proof.
	_, indistinguishable := e2eResolutionDynamicGuessVerdict(
		[]string{dynamicTo + " origin=text_matched tier="},
		[]string{staticTo + " origin=text_matched tier="})
	if len(indistinguishable) != 1 {
		t.Fatalf("a guess wearing exactly the static sibling's provenance was not reported: %v", indistinguishable)
	}

	// Different provenance: the branch does distinguish them, which is what
	// the case asks for.
	_, indistinguishable = e2eResolutionDynamicGuessVerdict(
		[]string{dynamicTo + " origin=text_matched tier="},
		[]string{staticTo + " origin=ast_resolved tier=certain"})
	if len(indistinguishable) != 0 {
		t.Fatalf("a guess labelled differently from the proof was reported as indistinguishable: %v", indistinguishable)
	}

	// Honest terminals are not guesses in any of the matrix's arms.
	for _, terminal := range []string{
		"issue767/dyn/app.py::run -calls-> unresolved::helper_value origin= tier=",
		"issue767/dyn/app.py::run -calls-> external::helper_value origin= tier=",
		"issue767/dyn/app.py::run -calls-> issue767::stdlib::importlib::import_module origin= tier=",
	} {
		if guesses, _ := e2eResolutionDynamicGuessVerdict([]string{terminal}, nil); len(guesses) != 0 {
			t.Errorf("%q was counted as a guess; an honest terminal is the shape a conservative binder is meant to emit", terminal)
		}
	}

	// A different target is not the same bind, whatever its provenance.
	_, indistinguishable = e2eResolutionDynamicGuessVerdict(
		[]string{dynamicTo + " origin=text_matched tier="},
		[]string{"issue767/dyn/app.py::call_helper -calls-> issue767/dyn/other.py::other origin=text_matched tier="})
	if len(indistinguishable) != 0 {
		t.Errorf("a proof of a DIFFERENT target was matched to the guess: %v", indistinguishable)
	}
}

func TestE2EMatrixResolutionDynamicEdgePartsSplitsKindTargetAndProvenance(t *testing.T) {
	kind, target, provenance := e2eResolutionDynamicEdgeParts("issue767/dyn/app.py::run -calls-> issue767/dyn/helper.py::helper_value origin=text_matched tier=")
	if kind != "calls" {
		t.Errorf("kind %q", kind)
	}
	if target != "issue767/dyn/helper.py::helper_value" {
		t.Errorf("target %q", target)
	}
	if provenance != "origin=text_matched tier=" {
		t.Errorf("provenance %q", provenance)
	}
	// A row without provenance must not silently become a row whose target
	// swallowed the provenance.
	if _, target, provenance = e2eResolutionDynamicEdgeParts("a -calls-> b"); target != "b" || provenance != "" {
		t.Errorf("a provenance-less row split to target %q provenance %q", target, provenance)
	}
}

func TestE2EMatrixResolutionSummariseDerivedKeepsComparedApartFromExercised(t *testing.T) {
	outputs := e2eResolutionOutputs()
	fresh, incremental := map[string][]string{}, map[string][]string{}
	ids := e2eResolutionDerivedOutputIDs()
	// One differing, one empty on both, the rest identical.
	for i, id := range ids {
		switch i {
		case 0:
			fresh[id], incremental[id] = []string{"a", "b"}, []string{"a"}
		case 1:
			// left empty on both arms
		default:
			fresh[id], incremental[id] = []string{"a"}, []string{"a"}
		}
	}
	summary := e2eResolutionSummariseDerived(fresh, incremental, outputs)
	if len(summary.Differing) != 1 || summary.Differing[0] != ids[0] {
		t.Errorf("differing: %v", summary.Differing)
	}
	if len(summary.Empty) != 1 || summary.Empty[0] != ids[1] {
		t.Errorf("empty: %v", summary.Empty)
	}
	if len(summary.Identical) != len(ids)-2 {
		t.Errorf("identical: %v", summary.Identical)
	}
	if summary.Compared() != len(ids)-1 {
		t.Errorf("compared %d, want %d", summary.Compared(), len(ids)-1)
	}
	described := summary.Describe()
	for _, want := range []string{"actually differ", "identical on both", "empty on both arms"} {
		if !strings.Contains(described, want) {
			t.Errorf("the description does not separate %q: %s", want, described)
		}
	}
	// A support projection is not one of the ten and must not be summarised.
	if strings.Contains(described, "nodes_and_locations") {
		t.Errorf("a support projection leaked into the derived summary: %s", described)
	}
}

func TestE2EMatrixResolutionValidateOutcomesRefusesASilentPass(t *testing.T) {
	cases := []e2eResolutionCase{
		{ID: "one", Gate: "G1", Needs: []string{"go.mod"}, What: "x", Falsifier: "the row this case names present when it must be absent"},
		{ID: "two", Gate: "G3", Needs: []string{"go.mod"}, What: "y", Falsifier: "the row this case names absent when it must be present"},
	}
	full := []e2eResolutionOutcome{{Case: "one", Status: e2eResolutionPass, Detail: "ok"}, {Case: "two", Status: e2eResolutionGap, Detail: "declared"}}
	if problems := e2eResolutionValidateOutcomes(cases, full); len(problems) != 0 {
		t.Fatalf("a complete table was rejected: %v", problems)
	}
	missing := e2eResolutionValidateOutcomes(cases, full[:1])
	if len(missing) != 1 || !strings.Contains(missing[0], "silent pass") {
		t.Fatalf("an unrecorded case was not reported as a silent pass: %v", missing)
	}
	twice := e2eResolutionValidateOutcomes(cases, append(append([]e2eResolutionOutcome(nil), full...), full[0]))
	if len(twice) != 1 || !strings.Contains(twice[0], "recorded 2 times") {
		t.Fatalf("a duplicated case was not reported: %v", twice)
	}
	undeclared := e2eResolutionValidateOutcomes(cases, append(append([]e2eResolutionOutcome(nil), full...), e2eResolutionOutcome{Case: "three", Status: e2eResolutionPass, Detail: "ok"}))
	if len(undeclared) != 1 || !strings.Contains(undeclared[0], "not declared") {
		t.Fatalf("an undeclared case was not reported: %v", undeclared)
	}
	badSkip := e2eResolutionValidateOutcomes(cases, []e2eResolutionOutcome{{Case: "one", Status: e2eResolutionNotExercised, Detail: e2eResolutionNothingAsserted}, full[1]})
	if len(badSkip) != 1 || !strings.Contains(badSkip[0], "ledger row") {
		t.Fatalf("a skip without a ledger row was accepted: %v", badSkip)
	}
	unknown := e2eResolutionValidateOutcomes(cases, []e2eResolutionOutcome{{Case: "one", Status: "green", Detail: "ok"}, full[1]})
	if len(unknown) != 1 || !strings.Contains(unknown[0], "unknown status") {
		t.Fatalf("an invented status was accepted: %v", unknown)
	}
	blank := e2eResolutionValidateOutcomes(cases, []e2eResolutionOutcome{{Case: "one", Status: e2eResolutionPass, Detail: "  "}, full[1]})
	if len(blank) != 1 || !strings.Contains(blank[0], "empty detail") {
		t.Fatalf("an outcome with no detail was accepted: %v", blank)
	}
}

func TestE2EMatrixResolutionRenderOutcomesShowsStatusGateAndDetail(t *testing.T) {
	rendered := e2eResolutionRenderOutcomes([]e2eResolutionOutcome{
		{Case: "manifest_only_invalidation", Gate: "G2", Hazard: "H8 manifest-only invalidation", Status: e2eResolutionPass, Detail: "sequence 4→4"},
		{Case: "context_separated_build_matches_fresh_index", Gate: "G1+G3", Status: e2eResolutionGap, Detail: "two declared gaps"},
	})
	for _, want := range []string{"manifest_only_invalidation", "H8 manifest-only invalidation", "G2", "sequence 4→4", e2eResolutionGap, "G1+G3"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the outcome table does not show %q:\n%s", want, rendered)
		}
	}
}

func TestE2EMatrixResolutionConfigTracksBothRepositoriesAndPutsTheSizeKnobWhereItIsRead(t *testing.T) {
	f := &issue767Fixture{root: filepath.Join("/private/tmp", "gx767-e2eres"), primary: filepath.Join("/private/tmp", "gx767-e2eres", "repo")}
	yaml := e2eResolutionConfigYAML(f)
	for _, want := range []string{
		strconv.Quote(f.primary),
		strconv.Quote(filepath.Join(f.root, e2eResolutionOtherRepoDir)),
		"name: " + issue767FixturePrefix,
		"name: " + e2eResolutionOtherRepoName,
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("the private configuration does not carry %q:\n%s", want, yaml)
		}
	}
	if strings.Contains(yaml, os.Getenv("HOME")+"/.gortex") {
		t.Error("the private configuration points at the user's own state")
	}
	// The cap must NOT be here. ConfigManager.GetRepoConfig
	// (internal/config/manager.go:281-286) returns the repository's own
	// .gortex.yaml or compiled defaults and never this file's `index:` block,
	// so a cap written here is reported by the run and absent from the
	// indexer — an oversized arm that measures an uncapped index.
	if strings.Contains(yaml, "max_file_size") {
		t.Error("index.max_file_size is in the daemon config, where GetRepoConfig never reads it; it belongs in the repository's .gortex.yaml")
	}
	// And it must be in the corpus's workspace config, at the value the
	// oversized arms report.
	workspace := ""
	for _, file := range e2eResolutionCorpusFiles() {
		if file.Path == ".gortex.yaml" {
			workspace = file.Content
		}
	}
	if workspace == "" {
		t.Fatal("the corpus declares no .gortex.yaml, so no cap reaches the indexer and every oversized arm is vacuous")
	}
	if !strings.Contains(workspace, "max_file_size: "+strconv.Itoa(e2eResolutionMaxFileSize)) {
		t.Errorf("the workspace config does not set the cap the oversized arms report (%d):\n%s", e2eResolutionMaxFileSize, workspace)
	}
}

func TestE2EMatrixResolutionSkipReasonNamesTheGateAndTheLedgerRow(t *testing.T) {
	if !strings.Contains(e2eResolutionSkipReason, e2eResolutionBinaryEnv) || !strings.Contains(e2eResolutionSkipReason, e2eResolutionLedgerRow) {
		t.Errorf("skip reason %q names neither the opt-in variable nor the ledger row", e2eResolutionSkipReason)
	}
}
