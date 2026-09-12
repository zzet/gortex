package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/persistence"
)

// ---------------------------------------------------------------------------
// 1. Registry: every production mutation entry point passes through the
//    authority, and an unregistered path fails.
// ---------------------------------------------------------------------------

// mutationDoorNames are the calls through which production source in this
// package reaches a generation-zero payload write: the coordinated entry point,
// every raw lane acquisition, and the lane-set acquisition the multi-repo cold
// batch uses.
//
// coordinateRepositoryReindex is deliberately absent: it COALESCES, so the
// queuing caller is not the work that executes. That door's receipt is held
// unconditionally by the lane worker (drain -> outputGeneration, bound for both
// lane flavours), which TestEveryMutationLaneIsBoundToTheAuthority pins.
var mutationDoorNames = map[string]bool{
	"coordinateRepositoryMutation":         true,
	"withOutputGeneration":                 true,
	"withOutputGenerationSource":           true,
	"withRepositoryOutputGenerationSource": true,
	"withRepositoryMutationLanes":          true,
	"runExclusive":                         true,
	"runExclusiveLaneOnly":                 true,
	"runExclusiveMode":                     true,
}

// authorityPlumbingDoorCount exempts the functions inside the authority's own
// file that DEFINE the doors rather than walk through one — each either takes
// the entry as a parameter or is the lane machinery the wrappers are bound to
// — and pins HOW MANY doors each of them contains.
//
// The count is the point. An exemption keyed on the function alone is the same
// per-function blind spot the per-door scan exists to remove, one file further
// in: a second receipt-less arm added inside one of these functions ("batch
// mode takes the lane only", which is exactly the shape that produced the
// topology-batch door) would inherit the exemption and never be seen. With the
// count pinned, any new door inside a plumbing function fails here until it is
// examined and the number is moved deliberately.
//
// The exemption is keyed on that file only: a plumbing-shaped name anywhere
// else is still scanned door by door.
var authorityPlumbingDoorCount = map[string]int{
	"coordinateRepositoryMutation":         2, // entry is a parameter: runExclusive + withOutputGeneration
	"coordinateRepositoryReindex":          0, // the lane worker holds the receipt
	"withRepositoryMutationLanes":          1, // lane acquisition; callers name entries
	"runExclusive":                         1,
	"runExclusiveLaneOnly":                 1,
	"runExclusiveMode":                     0,
	"withOutputGeneration":                 1, // entry is a parameter
	"withOutputGenerationSource":           0, // entry is a parameter
	"withRepositoryOutputGenerationSource": 0, // entry is a parameter
}

const authorityPlumbingFile = "repository_mutation_coordinator.go"

// mutationDoor is ONE call site, not one function. A function with two doors
// into the same payload (coordinateRepositoryTopologyMutation has a coordinated
// arm and a lane-only arm) is two rows here, and each must name its own entry:
// checking per function would let one named arm vouch for an unnamed sibling.
type mutationDoor struct {
	file    string
	fn      string
	door    string
	line    int
	entries []string
}

func (d mutationDoor) String() string {
	return fmt.Sprintf("%s:%d %s -> %s(...)", d.file, d.line, d.fn, d.door)
}

func mutationDoorCallee(expr ast.Expr) string {
	switch fun := expr.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// scanProductionMutationDoors parses the package's own non-test source and
// returns every mutation door with the OutputEntry identifiers named anywhere
// inside that door's own arguments (the body of the closure it is handed
// included, which is where a lane acquisition names its entry).
func scanProductionMutationDoors(t *testing.T) []mutationDoor {
	t.Helper()
	return scanMutationDoorsIn(t, ".")
}

// scanMutationDoorsIn is the scanner itself, parameterised by directory so the
// detector can be tested against deliberately broken source
// (TestMutationDoorScanCatchesAnUnnamedDoor) instead of only against source
// that happens to be correct today.
func scanMutationDoorsIn(t *testing.T, dir string) []mutationDoor {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob package source: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no package source found under %s", dir)
	}
	sort.Strings(files)
	fset := token.NewFileSet()
	var doors []mutationDoor
	for _, path := range files {
		file := filepath.Base(path)
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", file, parseErr)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				callee := mutationDoorCallee(call.Fun)
				if !mutationDoorNames[callee] {
					return true
				}
				var entries []string
				for _, arg := range call.Args {
					ast.Inspect(arg, func(inner ast.Node) bool {
						if ident, isIdent := inner.(*ast.Ident); isIdent &&
							strings.HasPrefix(ident.Name, "OutputEntry") {
							entries = append(entries, ident.Name)
						}
						return true
					})
				}
				doors = append(doors, mutationDoor{
					file:    file,
					fn:      fn.Name.Name,
					door:    callee,
					line:    fset.Position(call.Pos()).Line,
					entries: entries,
				})
				return true
			})
		}
	}
	return doors
}

// plumbingExemptionMismatches reports every exempt plumbing function whose door
// count no longer matches the pinned number. It is a pure function of the
// scanned doors so the guard itself can be tested against synthetic input
// (TestPlumbingExemptionCatchesAnAddedDoor) rather than only against source
// that happens to be correct today.
func plumbingExemptionMismatches(doors []mutationDoor, expected map[string]int) []string {
	counted := map[string]int{}
	for _, door := range doors {
		if door.file != authorityPlumbingFile {
			continue
		}
		if _, exempt := expected[door.fn]; exempt {
			counted[door.fn]++
		}
	}
	names := make([]string, 0, len(expected))
	for fn := range expected {
		names = append(names, fn)
	}
	sort.Strings(names)
	var mismatches []string
	for _, fn := range names {
		if got := counted[fn]; got != expected[fn] {
			mismatches = append(mismatches, fmt.Sprintf(
				"%s: %s contains %d mutation doors, the exemption covers %d; a door added inside a plumbing function must name its own entry, not inherit the exemption",
				authorityPlumbingFile, fn, got, expected[fn]))
		}
	}
	return mismatches
}

// TestPlumbingExemptionCatchesAnAddedDoor tests that guard.
//
// The exemption used to be keyed on the FUNCTION, which is the same
// per-function blind spot the per-door scan exists to remove, one file further
// in: a second receipt-less arm added inside one of the authority's own
// plumbing functions ("batch mode takes the lane only" — exactly the shape that
// produced the topology-batch door) would inherit the exemption and never be
// seen. Pinning the count is what makes that visible.
func TestPlumbingExemptionCatchesAnAddedDoor(t *testing.T) {
	expected := map[string]int{"coordinateRepositoryMutation": 2}
	pristine := []mutationDoor{
		{file: authorityPlumbingFile, fn: "coordinateRepositoryMutation", door: "runExclusive"},
		{file: authorityPlumbingFile, fn: "coordinateRepositoryMutation", door: "withOutputGeneration"},
	}
	if got := plumbingExemptionMismatches(pristine, expected); len(got) != 0 {
		t.Fatalf("a plumbing function at its pinned door count must not be reported: %v", got)
	}

	added := append(append([]mutationDoor(nil), pristine...), mutationDoor{
		file: authorityPlumbingFile, fn: "coordinateRepositoryMutation", door: "runExclusiveLaneOnly",
	})
	got := plumbingExemptionMismatches(added, expected)
	if len(got) != 1 || !strings.Contains(got[0], "contains 3 mutation doors") {
		t.Fatalf("an added door inside a plumbing function was not reported: %v", got)
	}

	removed := plumbingExemptionMismatches(pristine[:1], expected)
	if len(removed) != 1 {
		t.Fatalf("a removed door must move the pin deliberately too: %v", removed)
	}
}

// TestEveryProductionMutationEntryPointIsRegistered reads the package's own
// production source and fails when ANY ONE mutation door reaches generation
// zero without naming a registered output-generation entry point.
//
// This is the static half of "every mutation names exactly one output
// generation/owner through the authority". A new call site that forgets to
// declare itself — a new watcher trigger, a new lane acquisition, a second arm
// added beside an existing named one — fails here rather than silently becoming
// the one mutation path with no named owner.
func TestEveryProductionMutationEntryPointIsRegistered(t *testing.T) {
	declared := map[string]bool{}
	for name := range outputMutationEntryKinds {
		declared[string(name)] = true
	}

	doors := scanProductionMutationDoors(t)
	// Every door inside an exempt plumbing function is counted first, so the
	// exemption can be applied PER DOOR against a pinned number rather than
	// per function: an added arm inside one of them is reported here instead of
	// inheriting the exemption.
	for _, mismatch := range plumbingExemptionMismatches(doors, authorityPlumbingDoorCount) {
		t.Error(mismatch)
	}

	checked := 0
	for _, door := range doors {
		if door.file == authorityPlumbingFile {
			if _, exempt := authorityPlumbingDoorCount[door.fn]; exempt {
				continue
			}
		}
		checked++
		if len(door.entries) == 0 {
			t.Errorf("%s reaches the generation-zero mutation lane without naming an output-generation entry point", door)
			continue
		}
		for _, ident := range door.entries {
			value, ok := outputMutationEntryValue(ident)
			if !ok {
				t.Errorf("%s names %s, which is not a declared OutputMutationEntry constant", door, ident)
				continue
			}
			if !declared[value] {
				t.Errorf("%s names entry %q, which is not registered in outputMutationEntryKinds", door, value)
			}
		}
	}
	if checked < 15 {
		t.Fatalf("only %d mutation doors were scanned; the door catalogue no longer matches reality", checked)
	}

	// The doors that do NOT go through coordinateRepositoryMutation are the
	// ones a reader would miss, so they are named explicitly: losing the
	// receipt on any of them must fail here even if some sibling arm in the
	// same function still names an entry.
	required := []mutationDoor{
		{file: "multi.go", fn: "indexMultiRepo", door: "withRepositoryMutationLanes", entries: []string{"OutputEntryIndexMultiRepo"}},
		{file: "multi.go", fn: "IndexRepo", door: "runExclusive", entries: []string{"OutputEntryIndexRepo"}},
		{file: "multi.go", fn: "incrementalDiscoverRepo", door: "runExclusive", entries: []string{"OutputEntryIncrementalDiscoverRepo"}},
		{file: "repository_topology_batch.go", fn: "coordinateRepositoryTopologyMutation", door: "runExclusiveLaneOnly", entries: []string{"OutputEntryRepositoryTopology"}},
		{file: "repository_topology_batch.go", fn: "coordinateRepositoryTopologyMutation", door: "coordinateRepositoryMutation", entries: []string{"OutputEntryRepositoryTopology"}},
		{file: authorityPlumbingFile, fn: "bindMutationLaneAuthority", door: "withOutputGenerationSource", entries: []string{"OutputEntryRepositoryReconcileLane"}},
		{file: authorityPlumbingFile, fn: "repositoryMutationCoordinator", door: "withRepositoryOutputGenerationSource", entries: []string{"OutputEntryRepositoryReconcileLane"}},
	}
	for _, want := range required {
		found := false
		for _, door := range doors {
			if door.file != want.file || door.fn != want.fn || door.door != want.door {
				continue
			}
			for _, ident := range door.entries {
				if ident == want.entries[0] {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s: %s -> %s(...) no longer names %s; that door's payload has no output generation",
				want.file, want.fn, want.door, want.entries[0])
		}
	}
	t.Logf("checked %d production mutation doors (%d found in total)", checked, len(doors))
}

// TestMutationDoorScanCatchesAnUnnamedDoor tests the DETECTOR, not the source.
//
// A static scan over source that happens to be correct today proves only that
// it found no complaint; it does not prove it can complain. This test runs the
// same scanner over source that deliberately reproduces the two door shapes an
// earlier version of the scan missed:
//
//  1. a lane-set acquisition (withRepositoryMutationLanes) whose closure names
//     no entry — the multi-repo cold batch with its receipts deleted; and
//  2. a function with TWO doors where only one arm names an entry — the
//     topology batch with its lane-only receipt deleted, which a per-FUNCTION
//     scan would wave through on the strength of the surviving arm.
//
// Both must be reported as unnamed doors.
func TestMutationDoorScanCatchesAnUnnamedDoor(t *testing.T) {
	dir := t.TempDir()
	source := `package fixture

func namedDoor(idx *stub) error {
	return idx.coordinateRepositoryMutation(ctx, OutputEntryIndexFile, fn)
}

func twoArmsOneNamed(idx *stub) error {
	if batched {
		return idx.repositoryMutations().runExclusiveLaneOnly(ctx, fn)
	}
	return idx.coordinateRepositoryMutation(ctx, OutputEntryRepositoryTopology, fn)
}

func laneSetWithoutReceipts(mi *stub) error {
	return mi.withRepositoryMutationLanes(ctx, prefixes, func() error {
		return mi.writeEveryRepository()
	})
}
`
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(source), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	doors := scanMutationDoorsIn(t, dir)
	named := map[string]bool{}
	unnamed := map[string]bool{}
	for _, door := range doors {
		key := door.fn + "/" + door.door
		if len(door.entries) == 0 {
			unnamed[key] = true
			continue
		}
		named[key] = true
	}

	for _, want := range []string{
		"laneSetWithoutReceipts/withRepositoryMutationLanes",
		"twoArmsOneNamed/runExclusiveLaneOnly",
	} {
		if !unnamed[want] {
			t.Errorf("the scan did not report %s as an unnamed mutation door; doors=%v", want, doors)
		}
	}
	if !named["namedDoor/coordinateRepositoryMutation"] {
		t.Errorf("the scan did not credit a correctly named door; doors=%v", doors)
	}
	if !named["twoArmsOneNamed/coordinateRepositoryMutation"] {
		t.Errorf("the scan lost the named arm of a two-door function; doors=%v", doors)
	}
}

// outputMutationEntryValue resolves a source identifier to its declared value,
// so the scan compares against the registry rather than against spelling.
func outputMutationEntryValue(identifier string) (string, bool) {
	known := map[string]OutputMutationEntry{
		"OutputEntryIndexCtx":                OutputEntryIndexCtx,
		"OutputEntryIndexFile":               OutputEntryIndexFile,
		"OutputEntryEvictFile":               OutputEntryEvictFile,
		"OutputEntryReresolveFileScoped":     OutputEntryReresolveFileScoped,
		"OutputEntryDeferredPasses":          OutputEntryDeferredPasses,
		"OutputEntryRepositoryTopology":      OutputEntryRepositoryTopology,
		"OutputEntryIndexMultiRepo":          OutputEntryIndexMultiRepo,
		"OutputEntryIndexRepo":               OutputEntryIndexRepo,
		"OutputEntryIncrementalDiscoverRepo": OutputEntryIncrementalDiscoverRepo,
		"OutputEntryGitWatcherFinalize":      OutputEntryGitWatcherFinalize,
		"OutputEntryPollerFinalizeGitHead":   OutputEntryPollerFinalizeGitHead,
		"OutputEntryWatcherDirScan":          OutputEntryWatcherDirScan,
		"OutputEntryWatcherPatchGraph":       OutputEntryWatcherPatchGraph,
		"OutputEntryWatcherEnqueueReresolve": OutputEntryWatcherEnqueueReresolve,
		"OutputEntryCheckoutSourceMutation":  OutputEntryCheckoutSourceMutation,
		"OutputEntryRepositoryReconcileLane": OutputEntryRepositoryReconcileLane,
		"OutputEntryEnrichmentCorpus":        OutputEntryEnrichmentCorpus,
	}
	value, ok := known[identifier]
	return string(value), ok
}

// TestEveryMutationLaneIsBoundToTheAuthority covers the coalescing door the
// static scan deliberately leaves to the lane: BOTH lane flavours — the
// MultiIndexer-owned one and the orphan one a standalone Indexer mints — carry
// the receipt wrapper, so a reconcile that merges many callers into one
// execution still names exactly one output generation for that execution.
func TestEveryMutationLaneIsBoundToTheAuthority(t *testing.T) {
	standalone := New(graph.New(), newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(standalone.Close)
	orphan := standalone.repositoryMutations()
	orphan.mu.Lock()
	orphanBound := orphan.outputGeneration != nil
	orphan.mu.Unlock()
	if !orphanBound {
		t.Fatal("the orphan lane a standalone Indexer mints is not bound to the output-generation authority")
	}

	mi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	owned := mi.newPerRepoIndexer(config.IndexConfig{})
	t.Cleanup(owned.Close)
	owned.SetRepoPrefix("bound-repo")
	lane := owned.repositoryMutations()
	lane.mu.Lock()
	laneBound := lane.outputGeneration != nil
	lane.mu.Unlock()
	if !laneBound {
		t.Fatal("the MultiIndexer-owned lane is not bound to the output-generation authority")
	}
}

// TestAuthorityRefusesAnUnregisteredEntryPoint is the runtime half: a call site
// the registry does not know cannot open a receipt at all.
func TestAuthorityRefusesAnUnregisteredEntryPoint(t *testing.T) {
	authority := NewOutputGenerationAuthority(nil)
	_, err := authority.Begin(context.Background(), OutputMutationEntry("indexer.SomeNewTrigger"),
		OutputMutationTarget{Kind: OutputGenerationLegacy, OwnerKey: "root:/tmp/x"})
	if !errors.Is(err, ErrOutputMutationEntryUnregistered) {
		t.Fatalf("unregistered entry point: got %v, want ErrOutputMutationEntryUnregistered", err)
	}
	if got := authority.Stats().Issued; got != 0 {
		t.Fatalf("a refused entry point must not issue a receipt; issued=%d", got)
	}
}

// TestAuthorityRefusesATargetThatIsNotExactlyOneOutputGeneration pins the
// "exactly one output generation and owner" half, including the refusal that
// keeps generation zero from being named as a committed generation.
func TestAuthorityRefusesATargetThatIsNotExactlyOneOutputGeneration(t *testing.T) {
	authority := NewOutputGenerationAuthority(nil)
	ctx := context.Background()
	cases := []struct {
		name   string
		entry  OutputMutationEntry
		target OutputMutationTarget
	}{
		{"no owner", OutputEntryIndexFile, OutputMutationTarget{Kind: OutputGenerationLegacy}},
		{"no axis", OutputEntryIndexFile, OutputMutationTarget{OwnerKey: "root:/tmp/x"}},
		{
			"legacy claiming a committed generation number",
			OutputEntryIndexFile,
			OutputMutationTarget{Kind: OutputGenerationLegacy, OwnerKey: "root:/tmp/x", Generation: 7},
		},
		{
			"checkout without a complete identity",
			OutputEntryCheckoutSourceMutation,
			OutputMutationTarget{Kind: OutputGenerationCheckout, OwnerKey: "checkout:c1", Generation: 4},
		},
		{
			"checkout without a generation",
			OutputEntryCheckoutSourceMutation,
			OutputMutationTarget{Kind: OutputGenerationCheckout, OwnerKey: "checkout:c1", CheckoutID: "c1", Incarnation: "i1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := authority.Begin(ctx, tc.entry, tc.target); !errors.Is(err, ErrOutputMutationTargetInvalid) {
				t.Fatalf("got %v, want ErrOutputMutationTargetInvalid", err)
			}
		})
	}
	// The axis an entry point declares is part of the fence: a legacy entry
	// point cannot admit a checkout generation.
	_, err := authority.Begin(ctx, OutputEntryIndexFile, OutputMutationTarget{
		Kind: OutputGenerationCheckout, OwnerKey: "checkout:c1", CheckoutID: "c1", Incarnation: "i1", Generation: 4,
	})
	if !errors.Is(err, ErrOutputMutationTargetInvalid) {
		t.Fatalf("entry/axis mismatch: got %v, want ErrOutputMutationTargetInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// 2. Gate 6: superseded work cannot fulfil a newer mutation receipt.
// ---------------------------------------------------------------------------

func TestSupersededWorkCannotFulfilANewerMutationReceipt(t *testing.T) {
	authority := NewOutputGenerationAuthority(nil)
	ctx := context.Background()
	target := OutputMutationTarget{Kind: OutputGenerationLegacy, OwnerKey: "root:/tmp/repo", RepoPrefix: "repo"}

	older, err := authority.Begin(ctx, OutputEntryIndexFile, target)
	if err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	if older.Superseded() {
		t.Fatal("a receipt with no successor must not report itself superseded")
	}
	newer, err := authority.Begin(ctx, OutputEntryEvictFile, target)
	if err != nil {
		t.Fatalf("second receipt: %v", err)
	}
	if !older.Superseded() {
		t.Fatal("a newer mutation for the same owner must supersede the older receipt")
	}
	if err := older.Complete(); !errors.Is(err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("superseded fulfilment: got %v, want ErrOutputMutationReceiptSuperseded", err)
	}
	if err := newer.Complete(); err != nil {
		t.Fatalf("the surviving receipt must still fulfil its generation: %v", err)
	}
	// A different owner is a different output: it is never superseded by this one.
	other, err := authority.Begin(ctx, OutputEntryIndexFile,
		OutputMutationTarget{Kind: OutputGenerationLegacy, OwnerKey: "root:/tmp/other"})
	if err != nil {
		t.Fatalf("other owner: %v", err)
	}
	if err := other.Complete(); err != nil {
		t.Fatalf("an unrelated owner must fulfil normally: %v", err)
	}
	stats := authority.Stats()
	if stats.Issued != 3 || stats.Settled != 3 || stats.Superseded != 1 {
		t.Fatalf("stats = %+v, want issued=3 settled=3 superseded=1", stats)
	}
	if stats.LiveOwners != 0 {
		t.Fatalf("settled receipts must not retain owner state; live owners = %d", stats.LiveOwners)
	}
	// Settling twice is refused rather than double-counted — and refused with
	// its OWN identity: the first Complete fulfilled the generation, so
	// reporting the second as "superseded" would be a false identity.
	err = newer.Complete()
	if !errors.Is(err, ErrOutputMutationReceiptSettled) {
		t.Fatalf("second Complete: got %v, want ErrOutputMutationReceiptSettled", err)
	}
	if errors.Is(err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("a receipt that DID fulfil must not report supersession on a second settle: %v", err)
	}
	if got := authority.Stats().Settled; got != 3 {
		t.Fatalf("a refused second settle must not be counted: settled=%d, want 3", got)
	}
}

// TestSupersededWorkIsRefusedBeforeItWrites is the PREVENTIVE half of gate 6.
//
// Admission is not instantaneous: Begin blocks on the repository's raw-source
// gate, so a newer mutation for the same owner can be admitted between "this
// receipt was issued" and "this receipt's payload starts". A receipt that
// already lost its authority must never run its body at all — refusing only at
// Complete would mean the bytes were already written when the refusal is
// issued.
func TestSupersededWorkIsRefusedBeforeItWrites(t *testing.T) {
	authority := NewOutputGenerationAuthority(nil)
	ctx := context.Background()
	target := OutputMutationTarget{Kind: OutputGenerationLegacy, OwnerKey: "root:/tmp/repo", RepoPrefix: "repo"}

	older, err := authority.Begin(ctx, OutputEntryIndexFile, target)
	if err != nil {
		t.Fatalf("first receipt: %v", err)
	}
	newer, err := authority.Begin(ctx, OutputEntryEvictFile, target)
	if err != nil {
		t.Fatalf("second receipt: %v", err)
	}

	ran := false
	err = runUnderOutputReceipt(older, func() error {
		ran = true
		return nil
	})
	if !errors.Is(err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("superseded payload: got %v, want ErrOutputMutationReceiptSuperseded", err)
	}
	if ran {
		t.Fatal("a superseded receipt ran its mutation body; the fence must refuse BEFORE any byte moves")
	}

	// The surviving receipt still runs and still fulfils.
	ran = false
	if err := runUnderOutputReceipt(newer, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("the surviving receipt must run and fulfil: %v", err)
	}
	if !ran {
		t.Fatal("the surviving receipt did not run its mutation body")
	}

	// The batch form refuses the same way, before the batch writes anything.
	batchOlder, err := authority.Begin(ctx, OutputEntryIndexMultiRepo, target)
	if err != nil {
		t.Fatalf("batch receipt: %v", err)
	}
	batch := OutputMutationReceipts{batchOlder}
	if err := batch.Current(); err != nil {
		t.Fatalf("an uncontested batch must be current: %v", err)
	}
	contender, err := authority.Begin(ctx, OutputEntryIndexMultiRepo, target)
	if err != nil {
		t.Fatalf("batch contender: %v", err)
	}
	defer contender.Abandon()
	if err := batch.Current(); !errors.Is(err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("batch preventive check: got %v, want ErrOutputMutationReceiptSuperseded", err)
	}
	batch.Abandon()
}

// TestCoalescedLaneRefusalKeepsTheResultItProduced pins the other half of the
// reporting refusal: work that DID run must not have its result thrown away.
//
// Every caller coalesced onto one reconcile execution receives that
// execution's outcome. Nulling the result on an authority refusal hands every
// one of them "nil result, no explanation" for work that really ran, while the
// error alone already tells them the generation was not fulfilled.
func TestCoalescedLaneRefusalKeepsTheResultItProduced(t *testing.T) {
	produced := &IndexResult{FileCount: 7}
	refusal := fmt.Errorf("%w: entry %q owner %q", ErrOutputMutationReceiptSuperseded, OutputEntryIndexFile, "root:/tmp/repo")

	// The lane wrapper runs the executor and then refuses fulfilment — exactly
	// what a supersession during the payload does.
	outcome := executeRepositoryMutationUnderAuthority(
		func(fn func(observe func(*OutputSourceContent)) error) error {
			if err := fn(func(*OutputSourceContent) {}); err != nil {
				return err
			}
			return refusal
		},
		true,
		func(paths []string) (*IndexResult, error) { return produced, nil },
		[]string{"a.go"},
	)
	if !errors.Is(outcome.err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("outcome error = %v, want the supersession refusal", outcome.err)
	}
	if outcome.result != produced {
		t.Fatalf("the refusal discarded the result of work that ran: result=%v", outcome.result)
	}

	// A PREVENTIVE refusal never runs the executor, so it carries no result to
	// lose and must report none.
	executed := false
	outcome = executeRepositoryMutationUnderAuthority(
		func(fn func(observe func(*OutputSourceContent)) error) error { return refusal },
		true,
		func(paths []string) (*IndexResult, error) { executed = true; return produced, nil },
		[]string{"a.go"},
	)
	if executed {
		t.Fatal("a preventive refusal must not run the executor")
	}
	if outcome.result != nil || !errors.Is(outcome.err, ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("preventive refusal outcome = %+v, want no result and the refusal", outcome)
	}
}

// TestUnboundProductionLaneFailsClosed pins the fail-closed half of "every
// mutation names one output generation".
//
// A coalescing lane nobody bound to the authority used to fall through to the
// unfenced executor: it wrote generation zero with no receipt, no named owner,
// and no error. A lane a PRODUCTION factory minted must refuse instead. A lane
// a fixture hand-built keeps the unfenced path, because it never claimed to
// name an output generation in the first place.
func TestUnboundProductionLaneFailsClosed(t *testing.T) {
	produced := &IndexResult{FileCount: 3}
	executed := false
	executor := func([]string) (*IndexResult, error) { executed = true; return produced, nil }

	outcome := executeRepositoryMutationUnderAuthority(nil, true, executor, []string{"a.go"})
	if !errors.Is(outcome.err, ErrOutputMutationLaneUnbound) {
		t.Fatalf("unbound production lane: got %v, want ErrOutputMutationLaneUnbound", outcome.err)
	}
	if executed {
		t.Fatal("an unbound production lane wrote generation zero with no receipt")
	}

	outcome = executeRepositoryMutationUnderAuthority(nil, false, executor, []string{"a.go"})
	if outcome.err != nil || outcome.result != produced || !executed {
		t.Fatalf("a fixture lane must keep running unfenced: %+v", outcome)
	}
}

// TestProductionLanesAreBornBoundToTheAuthority proves the fail-closed refusal
// above can never fire in production: both factories that mint a stable lane
// bind it and mark it authority-required, so a reconcile can reach neither an
// unbound lane nor the refusal.
func TestProductionLanesAreBornBoundToTheAuthority(t *testing.T) {
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })

	// The MultiIndexer factory: a lane minted before any Indexer attaches.
	lane := mi.repositoryMutationCoordinator("lane-repo")
	lane.mu.Lock()
	bound, required := lane.outputGeneration != nil, lane.authorityRequired
	lane.mu.Unlock()
	if !bound {
		t.Fatal("a lane minted by MultiIndexer.repositoryMutationCoordinator is not bound to the authority")
	}
	if !required {
		t.Fatal("a production lane must require the authority, so losing the binding fails closed")
	}

	// The orphan factory: a standalone Indexer's own lane.
	standalone := New(graph.New(), newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(standalone.Close)
	orphan := standalone.repositoryMutations()
	orphan.mu.Lock()
	bound, required = orphan.outputGeneration != nil, orphan.authorityRequired
	orphan.mu.Unlock()
	if !bound || !required {
		t.Fatalf("the orphan lane is bound=%v required=%v, want both", bound, required)
	}
}

func TestClosedAuthorityRefusesAdmissionButLetsLiveWorkSettle(t *testing.T) {
	authority := NewOutputGenerationAuthority(nil)
	ctx := context.Background()
	target := OutputMutationTarget{Kind: OutputGenerationLegacy, OwnerKey: "root:/tmp/repo"}
	live, err := authority.Begin(ctx, OutputEntryIndexFile, target)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	authority.Close()
	if _, err := authority.Begin(ctx, OutputEntryIndexFile, target); !errors.Is(err, ErrOutputMutationAuthorityClosed) {
		t.Fatalf("admission after Close: got %v, want ErrOutputMutationAuthorityClosed", err)
	}
	if err := live.Complete(); err != nil {
		t.Fatalf("work already on a lane must still settle after Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 3. Production entry-point trace.
// ---------------------------------------------------------------------------

// newAuthorityTestIndexer builds a real Indexer over a real repository and a
// real authority, indexes it once through the production entry point, and
// returns both. Nothing here reaches the developer's daemon or store.
func newAuthorityTestIndexer(t *testing.T, leases *graphview.LeaseManager) (*Indexer, *OutputGenerationAuthority, string) {
	t.Helper()
	root := setupRepoDir(t, "authority-repo")
	authority := NewOutputGenerationAuthority(leases)
	idx := New(graph.New(), newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("authority-repo")
	idx.SetOutputGenerationAuthority(authority)
	if _, err := idx.IndexCtx(context.Background(), root); err != nil {
		t.Fatalf("IndexCtx: %v", err)
	}
	return idx, authority, root
}

// TestProductionMutationEntryPointsReachTheAuthority is the wiring proof: the
// public entry points a daemon actually calls — IndexCtx, IndexFile, EvictFile,
// IncrementalReindexPaths — each admit through the installed authority and each
// name the SAME owner, because they all write one repository's generation zero.
func TestProductionMutationEntryPointsReachTheAuthority(t *testing.T) {
	idx, authority, root := newAuthorityTestIndexer(t, nil)

	after := authority.Stats()
	if after.Issued == 0 {
		t.Fatal("IndexCtx did not admit through the authority")
	}
	baseline := after.Issued

	file := filepath.Join(root, "second.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc Second() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := idx.IndexFile(file); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	if _, err := idx.IncrementalReindexPaths(root, []string{file}); err != nil {
		t.Fatalf("IncrementalReindexPaths: %v", err)
	}
	if nodes, _ := idx.EvictFile(file); nodes == 0 {
		t.Fatal("EvictFile removed nothing; the fixture no longer exercises a point mutation")
	}

	stats := authority.Stats()
	if stats.Issued <= baseline {
		t.Fatalf("production entry points did not open receipts: issued %d -> %d", baseline, stats.Issued)
	}
	if stats.Settled != stats.Issued {
		t.Fatalf("every admitted mutation must settle: issued=%d settled=%d", stats.Issued, stats.Settled)
	}
	if stats.Superseded != 0 {
		t.Fatalf("serial mutations on one lane must never supersede each other: %+v", stats)
	}
	if stats.LiveOwners != 0 {
		t.Fatalf("no owner state may leak after the last mutation settled: %d", stats.LiveOwners)
	}

	// One owner for one corpus: the target every lane names is the repository
	// root, not a per-Indexer identity.
	target := idx.legacyOutputTarget()
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if !strings.HasSuffix(target.OwnerKey, "|root:"+filepath.Clean(abs)) {
		t.Fatalf("owner key = %q, want it to name the repository root", target.OwnerKey)
	}
	if !strings.HasPrefix(target.OwnerKey, "store:") {
		t.Fatalf("owner key = %q, want it to name the output store it writes", target.OwnerKey)
	}
	if target.Kind != OutputGenerationLegacy || target.Generation != 0 {
		t.Fatalf("a legacy mutation must name generation zero: %+v", target)
	}
}

// TestIndexerResolvesTheOwningMultiIndexerAuthority proves the orphan lane and
// the owned lane resolve to ONE authority: a per-repository Indexer with no
// authority of its own falls through to the MultiIndexer the stack installed on.
func TestIndexerResolvesTheOwningMultiIndexerAuthority(t *testing.T) {
	authority := NewOutputGenerationAuthority(nil)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, newTestConfigManager(t), zap.NewNop())
	mi.SetOutputGenerationAuthority(authority)
	owned := mi.newPerRepoIndexer(config.IndexConfig{})
	t.Cleanup(owned.Close)
	if got := owned.outputGenerationAuthority(); got != authority {
		t.Fatal("a per-repository Indexer must resolve to the MultiIndexer's installed authority")
	}
	standalone := New(graph.New(), newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(standalone.Close)
	if got := standalone.outputGenerationAuthority(); got == authority {
		t.Fatal("an Indexer nobody installed on must not silently borrow another stack's authority")
	}
	standalone.SetOutputGenerationAuthority(authority)
	if got := standalone.outputGenerationAuthority(); got != authority {
		t.Fatal("an explicit installation must win")
	}
}

// TestOutputOwnerIsTheStoreNotOnlyTheRepository is a regression test for two
// legitimate concurrent mutations falsely superseding each other.
//
// SparseGenerationBuilder.runPass builds a PRIVATE Indexer over its own store
// handle carrying the live repository's prefix, and indexes the tree into it.
// That payload is a different output from the live corpus, and two such builds
// for one prefix are different outputs again — so the output owner must name
// the store, not only the repository. Keying it on the repository alone made
// dormancy activation (a generation build beside the eager view) fail with
// ErrOutputMutationReceiptSuperseded for work that was never in conflict.
//
// The case the fence DOES exist for must keep colliding: two Indexers over the
// SAME store and the same repository are one owner.
func TestOutputOwnerIsTheStoreNotOnlyTheRepository(t *testing.T) {
	root := setupRepoDir(t, "shared-root")
	live := graph.New()

	primary := New(live, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(primary.Close)
	primary.SetRepoPrefix("shared-prefix")
	if _, err := primary.IndexCtx(context.Background(), root); err != nil {
		t.Fatalf("primary IndexCtx: %v", err)
	}

	// The orphan-lane Indexer a stack hands the MCP server: same store, same
	// repository, different object. One output owner.
	orphan := New(live, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(orphan.Close)
	orphan.SetRepoPrefix("shared-prefix")
	if _, err := orphan.IndexCtx(context.Background(), root); err != nil {
		t.Fatalf("orphan IndexCtx: %v", err)
	}
	if primary.legacyOutputTarget().OwnerKey != orphan.legacyOutputTarget().OwnerKey {
		t.Fatalf("two Indexers over one store and one repository must be ONE owner: %q vs %q",
			primary.legacyOutputTarget().OwnerKey, orphan.legacyOutputTarget().OwnerKey)
	}

	// A generation build's private handle is a DIFFERENT output.
	private := New(graph.New(), newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	t.Cleanup(private.Close)
	private.SetRepoPrefix("shared-prefix")
	if _, err := private.IndexCtx(context.Background(), root); err != nil {
		t.Fatalf("generation-handle IndexCtx: %v", err)
	}
	if private.legacyOutputTarget().OwnerKey == primary.legacyOutputTarget().OwnerKey {
		t.Fatalf("a private generation handle must not share the live corpus owner: %q",
			private.legacyOutputTarget().OwnerKey)
	}

	// End to end through the authority: the private build and the live corpus
	// mutate concurrently under one authority and neither is superseded.
	authority := NewOutputGenerationAuthority(nil)
	primary.SetOutputGenerationAuthority(authority)
	private.SetOutputGenerationAuthority(authority)
	file := filepath.Join(root, "concurrent.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc Concurrent() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := private.IndexCtx(context.Background(), root); err != nil {
		t.Fatalf("generation build under the authority: %v", err)
	}
	if err := primary.IndexFile(file); err != nil {
		t.Fatalf("live corpus mutation under the authority: %v", err)
	}
	if stats := authority.Stats(); stats.Superseded != 0 {
		t.Fatalf("independent outputs must not supersede each other: %+v", stats)
	}
}

// ---------------------------------------------------------------------------
// 4. The source authority: a generation-zero mutation is witnessed, so a live
//    BasePin reports the corpus moved.
// ---------------------------------------------------------------------------

// TestLegacyMutationMovesTheBaseCorpusWitnessUnderALivePin is the end-to-end
// proof of the mutation-label half of W5.3.
//
// graphview.BasePin is what a routed request holds for its lifetime, and
// internal/mcp/view_request.go:210 turns exactly ErrBaseCorpusChanged into the
// request's base_changed rider (the answer stops claiming exactness). Before
// this item no production caller opened a source authority, so ValidateCurrent
// could only ever answer "unwitnessed" and the label was inert. Here the pin is
// taken first, a real production mutation entry point runs, and the pin reports
// the change.
func TestLegacyMutationMovesTheBaseCorpusWitnessUnderALivePin(t *testing.T) {
	leases := graphview.NewLeaseManager()
	idx, authority, root := newAuthorityTestIndexer(t, leases)

	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	canonical := filepath.Clean(abs)
	registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{
		RepoPrefix:   "authority-repo",
		RootIdentity: canonical,
		Incarnation:  "inc-1",
	}, nil)
	if err != nil {
		t.Fatalf("register raw source owner: %v", err)
	}
	if _, err := leases.CaptureInitialRawRepositorySource(context.Background(), registration, "initial"); err != nil {
		t.Fatalf("capture initial source: %v", err)
	}

	// A routed request pins the base corpus it is about to read.
	pin := leases.AcquireBaseCorpus("authority-repo")
	defer pin.Release()
	if !pin.Witnessed() {
		t.Fatal("a registered source authority must give the pin a witness to compare against")
	}
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("an unmutated corpus must validate clean: %v", err)
	}

	// A production mutation entry point runs while the request is live.
	file := filepath.Join(root, "witnessed.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc Witnessed() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := idx.IndexFile(file); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}

	if got := authority.Stats().Witnessed; got == 0 {
		t.Fatal("the mutation did not open the repository's source authority")
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, graphview.ErrBaseCorpusChanged) {
		t.Fatalf("pin after a witnessed generation-zero mutation: got %v, want ErrBaseCorpusChanged", err)
	}

	// A pin taken after the mutation sees the new state as stable again, so the
	// label reports movement, not permanent suspicion.
	fresh := leases.AcquireBaseCorpus("authority-repo")
	defer fresh.Release()
	if err := fresh.ValidateCurrent(); err != nil {
		t.Fatalf("a pin taken after the mutation must validate clean: %v", err)
	}
}

// TestUnwitnessedRepositoryIsReportedAsUnknownNotUnchanged pins the honesty
// half: a repository with no registered source authority must never be
// reported as unchanged, and the authority must not invent a witness for it.
func TestUnwitnessedRepositoryIsReportedAsUnknownNotUnchanged(t *testing.T) {
	leases := graphview.NewLeaseManager()
	idx, authority, root := newAuthorityTestIndexer(t, leases)

	pin := leases.AcquireBaseCorpus("authority-repo")
	defer pin.Release()
	if pin.Witnessed() {
		t.Fatal("no raw source owner is registered; the pin must carry no witness")
	}
	file := filepath.Join(root, "unwitnessed.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc Unwitnessed() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := idx.IndexFile(file); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	if got := authority.Stats().Witnessed; got != 0 {
		t.Fatalf("the authority must not manufacture a witness it did not take: witnessed=%d", got)
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, graphview.ErrBaseCorpusUnwitnessed) {
		t.Fatalf("unwitnessed corpus: got %v, want ErrBaseCorpusUnwitnessed", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Plan-bookkeeping obligation B1: internal/persistence is outside the
//    payload authority.
// ---------------------------------------------------------------------------

// TestRepositoryCleanupLeavesPersistenceSidecarsAlone pins both halves of B1.
//
// internal/persistence (notes, memories, scopes, notebooks, feedback) is a
// SEPARATE sidecar database with no generation axis. It is never an output
// generation, it is never named by an OutputMutationTarget, and repository
// untrack / retirement / payload cleanup must never treat it as payload.
func TestRepositoryCleanupLeavesPersistenceSidecarsAlone(t *testing.T) {
	// (a) the import boundary: the packages that own payload cleanup do not
	// even link the sidecar package, so they cannot delete or rewrite it.
	for _, dir := range []string{".", "../graph/store_sqlite"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		if len(files) == 0 {
			t.Fatalf("no source found under %s", dir)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			raw, readErr := os.ReadFile(file)
			if readErr != nil {
				t.Fatalf("read %s: %v", file, readErr)
			}
			if strings.Contains(string(raw), `"github.com/zzet/gortex/internal/persistence"`) {
				t.Errorf("%s imports internal/persistence: the sidecar is outside the payload authority and must not be reachable from payload cleanup", file)
			}
		}
	}

	// (b) the on-disk fact. The sidecars are written into the SAME directory
	// the graph store the MultiIndexer writes through lives in — which is where
	// production puts them (persistence.DefaultSidecarPath(dataDir) is
	// "<dataDir>/sidecar.sqlite", beside the graph database) — so the cleanup
	// path under test genuinely operates in this directory. The test then
	// asserts BOTH that the directory changed (proving it is live under the
	// code under test, not an unreachable scratch dir) and that every sidecar
	// in it is byte-identical afterwards.
	repoA := setupRepoDir(t, "sidecar-a")
	repoB := setupRepoDir(t, "sidecar-b")
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoA, Name: "sidecar-a"},
		{Path: repoB, Name: "sidecar-b"},
	}}
	gc.SetConfigPath(tmpCfg)
	if err := gc.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	cm, err := config.NewConfigManager(tmpCfg)
	if err != nil {
		t.Fatalf("config manager: %v", err)
	}

	dataDir := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(dataDir, "graph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The primary name is the production one, read from the package that owns
	// it rather than spelled out here: a rename of the sidecar would otherwise
	// leave this test guarding a file nothing writes.
	productionSidecar := filepath.Base(persistence.DefaultSidecarPath(dataDir))
	if filepath.Dir(persistence.DefaultSidecarPath(dataDir)) != dataDir {
		t.Fatalf("the production sidecar no longer lives beside the graph store: %q",
			persistence.DefaultSidecarPath(dataDir))
	}
	sidecars := map[string][]byte{
		productionSidecar:  []byte("notes + memories + notebooks payload that no generation owns"),
		"memories.sqlite":  []byte("memories payload that no generation owns"),
		"notebooks.sqlite": []byte("notebook payload that no generation owns"),
		"scopes.sqlite":    []byte("scope payload that no generation owns"),
	}
	before := map[string]string{}
	for name, body := range sidecars {
		path := filepath.Join(dataDir, name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write sidecar: %v", err)
		}
		before[name] = fileDigest(t, path)
	}
	payloadBefore := payloadDirectoryFingerprint(t, dataDir, sidecars)

	mi := NewMultiIndexer(store, newTestRegistry(), nil, cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	if _, err := mi.IndexAll(); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	if _, _, err := mi.UntrackRepoChecked(context.Background(), "sidecar-b"); err != nil {
		t.Fatalf("UntrackRepoChecked: %v", err)
	}

	if payloadDirectoryFingerprint(t, dataDir, sidecars) == payloadBefore {
		t.Fatal("nothing in the store directory changed; the sidecars were never within reach of the code under test and the assertion below would be vacuous")
	}

	names := make([]string, 0, len(sidecars))
	for name := range sidecars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dataDir, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("repository cleanup removed the sidecar %s: %v", name, err)
		}
		if got := fileDigest(t, path); got != before[name] {
			t.Fatalf("repository cleanup rewrote the sidecar %s", name)
		}
	}
}

// payloadDirectoryFingerprint summarises everything in dir that is NOT one of
// the sidecars: the payload the store owns. A changed fingerprint is the proof
// that the directory is live under the code under test.
func payloadDirectoryFingerprint(t *testing.T, dir string, sidecars map[string][]byte) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, isSidecar := sidecars[entry.Name()]; isSidecar {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			t.Fatalf("stat %s: %v", entry.Name(), statErr)
		}
		parts = append(parts, fmt.Sprintf("%s:%d", entry.Name(), info.Size()))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// TestDuplicateTrackedEntriesForOneRepositoryRootTakeOneReceipt is a
// regression test for a cold index that fully succeeded being reported as
// superseded.
//
// Two config entries can name ONE repository root: resolveTrackPrefix honours
// an explicit Name verbatim, so two entries with different Names and one Path
// survive the prefix-collision guard as two resolved repositories.
// legacyOutputTargetFor keys the output owner on the ROOT, so both name one
// generation-zero output. Issuing a receipt per resolved repository made the
// second supersede the first, and the batch then refused its own completed
// work with ErrOutputMutationReceiptSuperseded.
func TestDuplicateTrackedEntriesForOneRepositoryRootTakeOneReceipt(t *testing.T) {
	root := setupRepoDir(t, "dupe-root")
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: root, Name: "dupe-one"},
		{Path: root, Name: "dupe-two"},
	}}
	gc.SetConfigPath(tmpCfg)
	if err := gc.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	cm, err := config.NewConfigManager(tmpCfg)
	if err != nil {
		t.Fatalf("config manager: %v", err)
	}

	authority := NewOutputGenerationAuthority(nil)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	mi.SetOutputGenerationAuthority(authority)

	results, err := mi.IndexAll()
	if err != nil {
		// The regression: a cold index that fully succeeded refused itself with
		// ErrOutputMutationReceiptSuperseded.
		t.Fatalf("IndexAll over two entries for one root: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("IndexAll produced no results")
	}
	stats := authority.Stats()
	if stats.Superseded != 0 {
		t.Fatalf("one repository root is ONE output owner; stats = %+v", stats)
	}
	if stats.Settled != stats.Issued || stats.Issued == 0 {
		t.Fatalf("every admitted receipt must settle: %+v", stats)
	}
	if stats.LiveOwners != 0 {
		t.Fatalf("no owner state may leak after the batch settled: %+v", stats)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// 6. The SOURCE half, wired: a legacy mutation of a repository a daemon
//    actually tracks moves that repository's source witness.
// ---------------------------------------------------------------------------

// TestTrackedRepositoryMutationMovesTheBaseWitnessThroughIndexAll is the
// production trace the earlier round did not have.
//
// The lifecycle registers exactly ONE owner per tracked repository prefix, and
// it is a DEDICATED owner (bindDedicatedGraph -> RegisterRepositoryOwner ->
// LeaseManager.RegisterRepositoryOwnerPrepared). Nothing in the tree registers
// a RAW repository owner, so a witness addressed by raw registration handle
// could never be found in a real daemon: Stats().Witnessed stayed 0 and every
// BasePin answered "unwitnessed" no matter how many generation-zero mutations
// ran. Here the owner is registered the way production registers it, a routed
// request pins the corpus, MultiIndexer.IndexAll runs, and the pin reports the
// change.
func TestTrackedRepositoryMutationMovesTheBaseWitnessThroughIndexAll(t *testing.T) {
	root := setupRepoDir(t, "witness-repo")
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: "witness-repo"}}}
	gc.SetConfigPath(tmpCfg)
	if err := gc.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	cm, err := config.NewConfigManager(tmpCfg)
	if err != nil {
		t.Fatalf("config manager: %v", err)
	}

	leases := graphview.NewLeaseManager()
	// Exactly what CheckoutLifecycle.RegisterRepositoryOwner publishes.
	if err := leases.RegisterRepositoryOwner(graphview.RepositoryOwner{
		GraphID:     "graph-witness-repo",
		CheckoutID:  "checkout-witness-repo",
		Incarnation: "inc-1",
		RepoPrefix:  "witness-repo",
	}); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}

	authority := NewOutputGenerationAuthority(leases)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	mi.SetOutputGenerationAuthority(authority)

	pin := leases.AcquireBaseCorpus("witness-repo")
	defer pin.Release()
	if !pin.OwnerPinned() {
		t.Fatal("the registered repository owner was not pinned by the base pin")
	}

	if _, err := mi.IndexAll(); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}

	if got := authority.Stats().Witnessed; got == 0 {
		t.Fatal("a cold index of a tracked repository did not move its source witness; the SOURCE half is unwired")
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, graphview.ErrBaseCorpusChanged) {
		t.Fatalf("base pin after a tracked-repository mutation: got %v, want ErrBaseCorpusChanged", err)
	}

	// A pin taken after the mutation sees a stable corpus again: the label
	// reports movement, not permanent suspicion.
	fresh := leases.AcquireBaseCorpus("witness-repo")
	defer fresh.Release()
	if !fresh.Witnessed() {
		t.Fatal("the mutation left no witness for the next request to compare against")
	}
	if err := fresh.ValidateCurrent(); err != nil {
		t.Fatalf("a pin taken after the mutation must validate clean: %v", err)
	}
}

// TestUnregisteredRepositoryStaysUnwitnessed is the other direction: the new
// prefix-keyed door must not manufacture a witness for a repository no owner
// is registered for, and must not refuse the mutation either.
func TestUnregisteredRepositoryStaysUnwitnessed(t *testing.T) {
	leases := graphview.NewLeaseManager()
	idx, authority, root := newAuthorityTestIndexer(t, leases)
	file := filepath.Join(root, "unregistered.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc Unregistered() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := idx.IndexFile(file); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}
	if got := authority.Stats().Witnessed; got != 0 {
		t.Fatalf("no owner is registered; the authority must take no witness: witnessed=%d", got)
	}
}

// TestWitnessedNoOpDoesNotReportTheCorpusMoved is the content-fingerprint
// claim.
//
// The source gate allocates a revision at ADMISSION, before anybody can know
// whether the payload will move a byte. A fingerprint derived from that
// revision therefore moves for every admitted mutation, so once the witness is
// wired every watcher tick over an unchanged repository would tell every live
// request its base corpus changed. The fingerprint is derived from CONTENT —
// the mutated path set and those paths' on-disk identities — so a pass that
// wrote nothing restores the observation readers already hold.
func TestWitnessedNoOpDoesNotReportTheCorpusMoved(t *testing.T) {
	leases := graphview.NewLeaseManager()
	if err := leases.RegisterRepositoryOwner(graphview.RepositoryOwner{
		GraphID: "graph-noop", CheckoutID: "checkout-noop", Incarnation: "inc-1", RepoPrefix: "authority-repo",
	}); err != nil {
		t.Fatalf("RegisterRepositoryOwner: %v", err)
	}
	idx, authority, root := newAuthorityTestIndexer(t, leases)

	file := filepath.Join(root, "steady.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc Steady() {}\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// One real pass establishes the witness the no-op below must preserve.
	if _, err := idx.IncrementalReindexPaths(root, []string{file}); err != nil {
		t.Fatalf("seed reindex: %v", err)
	}

	pin := leases.AcquireBaseCorpus("authority-repo")
	defer pin.Release()
	if !pin.Witnessed() {
		t.Fatal("the seeding mutation left no witness")
	}
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("the pin must start clean: %v", err)
	}
	unchangedBefore := authority.Stats().Unchanged

	// The watcher tick: the same path, nothing on disk changed.
	result, err := idx.IncrementalReindexPaths(root, []string{file})
	if err != nil {
		t.Fatalf("no-op reindex: %v", err)
	}
	if result == nil || result.StaleFileCount != 0 || result.DeletedFileCount != 0 || result.FullRetrack {
		t.Fatalf("the fixture no longer reproduces a no-op pass: %+v", result)
	}
	if got := authority.Stats().Unchanged; got != unchangedBefore+1 {
		t.Fatalf("a no-op pass must restore the source witness: unchanged %d -> %d", unchangedBefore, got)
	}
	if err := pin.ValidateCurrent(); err != nil {
		t.Fatalf("a witnessed no-op told a live request the corpus moved: %v", err)
	}

	// A pass that DOES move content still reports movement.
	if err := os.WriteFile(file, []byte("package main\n\nfunc Steady() {}\n\nfunc Moved() {}\n"), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	if _, err := idx.IncrementalReindexPaths(root, []string{file}); err != nil {
		t.Fatalf("moving reindex: %v", err)
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, graphview.ErrBaseCorpusChanged) {
		t.Fatalf("a pass that rewrote a file must report movement: got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 7. Runtime pins for the doors that do NOT go through
//    coordinateRepositoryMutation.
// ---------------------------------------------------------------------------

// TestColdBatchOpensOneReceiptPerRepositoryOwner pins the multi-repo cold
// batch at RUNTIME, not only through the static scan: the batch admits one
// receipt per output owner under its own entry point, and every one of them
// settles.
func TestColdBatchOpensOneReceiptPerRepositoryOwner(t *testing.T) {
	repoA := setupRepoDir(t, "batch-a")
	repoB := setupRepoDir(t, "batch-b")
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoA, Name: "batch-a"},
		{Path: repoB, Name: "batch-b"},
	}}
	gc.SetConfigPath(tmpCfg)
	if err := gc.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	cm, err := config.NewConfigManager(tmpCfg)
	if err != nil {
		t.Fatalf("config manager: %v", err)
	}

	authority := NewOutputGenerationAuthority(nil)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, cm, zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	mi.SetOutputGenerationAuthority(authority)

	if _, err := mi.IndexAll(); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	stats := authority.Stats()
	if got := stats.Entries[OutputEntryIndexMultiRepo]; got != 2 {
		t.Fatalf("the cold batch opened %d receipts under %s, want one per repository owner (2)",
			got, OutputEntryIndexMultiRepo)
	}
	if stats.Superseded != 0 || stats.Settled != stats.Issued {
		t.Fatalf("every batch receipt must settle cleanly: %+v", stats)
	}
}

// TestTopologyBatchLaneOpensItsOwnReceipt pins BOTH arms of
// coordinateRepositoryTopologyMutation at runtime: the coordinated arm and the
// lane-only arm a topology batch takes. The lane-only arm is the door an
// earlier per-function scan waved through on the strength of its sibling.
func TestTopologyBatchLaneOpensItsOwnReceipt(t *testing.T) {
	authority := NewOutputGenerationAuthority(nil)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { _ = mi.Close(context.Background()) })
	mi.SetOutputGenerationAuthority(authority)
	idx := mi.newPerRepoIndexer(config.IndexConfig{})
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("topology-repo")

	ran := 0
	// The coordinated arm: no active topology batch in the context.
	if err := mi.coordinateRepositoryTopologyMutation(context.Background(), idx, func() error {
		ran++
		return nil
	}); err != nil {
		t.Fatalf("coordinated topology mutation: %v", err)
	}
	if got := authority.Stats().Entries[OutputEntryRepositoryTopology]; got != 1 {
		t.Fatalf("the coordinated arm opened %d receipts, want 1", got)
	}

	// The lane-only arm: inside a topology batch.
	if err := mi.RunRepositoryTopologyBatch(context.Background(), func(batchCtx context.Context) error {
		return mi.coordinateRepositoryTopologyMutation(batchCtx, idx, func() error {
			ran++
			return nil
		})
	}); err != nil {
		t.Fatalf("batched topology mutation: %v", err)
	}
	if ran != 2 {
		t.Fatalf("both arms must run their payload: ran=%d", ran)
	}
	stats := authority.Stats()
	if got := stats.Entries[OutputEntryRepositoryTopology]; got != 2 {
		t.Fatalf("the lane-only arm did not open a receipt: %s count = %d, want 2",
			OutputEntryRepositoryTopology, got)
	}
	if stats.Settled != stats.Issued || stats.Superseded != 0 {
		t.Fatalf("both topology receipts must settle cleanly: %+v", stats)
	}
}

// TestOutputStoreIdentityIsAMonotonicIdNotAnAddress pins the output half of the
// owner key.
//
// An address is unique only among LIVE objects: a closed store whose memory a
// new store of the same type reuses would inherit its identity, and two
// genuinely independent outputs would then supersede each other. The id is
// issued once per store and never reused.
func TestOutputStoreIdentityIsAMonotonicIdNotAnAddress(t *testing.T) {
	first := graph.New()
	second := graph.New()

	idFirst := outputStoreIdentity(first)
	if idFirst != outputStoreIdentity(first) {
		t.Fatal("one store must keep one identity for its whole life")
	}
	if idFirst == outputStoreIdentity(second) {
		t.Fatal("two stores must be two outputs")
	}
	if outputStoreIdentity(nil) != "store:none" {
		t.Fatalf("a nil store: %q", outputStoreIdentity(nil))
	}

	// The identity must be an issued NUMBER, not a formatted address: %p
	// renders "0x..." and would not parse.
	for _, identity := range []string{idFirst, outputStoreIdentity(second)} {
		slash := strings.LastIndex(identity, "/")
		if slash < 0 || !strings.HasPrefix(identity, "store:") {
			t.Fatalf("store identity %q is not store:<type>/<id>", identity)
		}
		issued, err := strconv.ParseUint(identity[slash+1:], 10, 64)
		if err != nil {
			t.Fatalf("store identity %q does not carry a monotonically issued id: %v", identity, err)
		}
		if issued == 0 {
			t.Fatalf("store identity %q carries no id", identity)
		}
	}
}

// TestOutputStoreIdentityRetiresADeadStore is the memory-hygiene half of the
// monotonic id.
//
// The registry is keyed by the handle's ADDRESS and holds no reference to the
// store, so a daemon that builds a private generation handle per sparse build
// does not accumulate closed stores. Retiring the entry is also what makes the
// id safe: the runtime frees an object only after its finalizer has run, so an
// address can never be handed to a new store while the previous store's entry
// is still in the map.
func TestOutputStoreIdentityRetiresADeadStore(t *testing.T) {
	outputStoreIDs.mu.Lock()
	before := len(outputStoreIDs.ids)
	outputStoreIDs.mu.Unlock()

	func() {
		transient := graph.New()
		if outputStoreIdentity(transient) == "" {
			t.Error("no identity issued")
		}
	}()

	// Finalizers run on their own goroutine after a collection, so the entry
	// disappears a short time after the GC rather than during it.
	for attempt := 0; attempt < 50; attempt++ {
		runtime.GC()
		outputStoreIDs.mu.Lock()
		now := len(outputStoreIDs.ids)
		outputStoreIDs.mu.Unlock()
		if now <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	outputStoreIDs.mu.Lock()
	now := len(outputStoreIDs.ids)
	outputStoreIDs.mu.Unlock()
	t.Fatalf("a dead store's identity was never retired: entries %d -> %d", before, now)
}

// TestMutationUnderAClosingOwnerStillMovesALivePin is the false-exact guard on
// the witness door.
//
// Closing an admission refuses NEW leases; it does not invalidate the pins
// already out, and finalization cannot run until they drain, so a request that
// pinned the base corpus before the close is still reading and still answering
// — internal/mcp/view_request.go treats a pin whose ValidateCurrent() is nil as
// "the answer is as exact as the route said it was". A real generation-zero
// write admitted while the owner is closing therefore may not be reported as
// unwitnessed: the corpus under that request has moved, and the honest
// alternative (ErrBaseCorpusUnwitnessed) is not available once the pin holds a
// witness. The observation is moved without a lease instead.
func TestMutationUnderAClosingOwnerStillMovesALivePin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, leases *graphview.LeaseManager, owner graphview.RepositoryOwner)
	}{
		{
			name: "closing owner",
			close: func(t *testing.T, leases *graphview.LeaseManager, owner graphview.RepositoryOwner) {
				if _, err := leases.CloseRepositoryAdmission(owner); err != nil {
					t.Fatalf("CloseRepositoryAdmission: %v", err)
				}
			},
		},
		{
			name: "stopped manager",
			close: func(t *testing.T, leases *graphview.LeaseManager, owner graphview.RepositoryOwner) {
				leases.ShutdownRepositoryAdmissions()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leases := graphview.NewLeaseManager()
			owner := graphview.RepositoryOwner{
				GraphID: "graph-closing", CheckoutID: "checkout-closing",
				Incarnation: "inc-1", RepoPrefix: "authority-repo",
			}
			if err := leases.RegisterRepositoryOwner(owner); err != nil {
				t.Fatalf("RegisterRepositoryOwner: %v", err)
			}
			idx, authority, root := newAuthorityTestIndexer(t, leases)
			seed := filepath.Join(root, "seed.go")
			if err := os.WriteFile(seed, []byte("package main\n\nfunc Seed() {}\n"), 0o644); err != nil {
				t.Fatalf("write seed: %v", err)
			}
			// One real pass establishes the witness the pin compares against.
			if _, err := idx.IncrementalReindexPaths(root, []string{seed}); err != nil {
				t.Fatalf("seed reindex: %v", err)
			}

			pin := leases.AcquireBaseCorpus("authority-repo")
			defer pin.Release()
			if !pin.Witnessed() {
				t.Fatal("the seeding mutation left no witness for the pin to compare against")
			}
			if err := pin.ValidateCurrent(); err != nil {
				t.Fatalf("the pin must start clean: %v", err)
			}

			tc.close(t, leases, owner)
			if err := pin.ValidateCurrent(); err != nil {
				t.Fatalf("closing an admission is not itself a source change: %v", err)
			}
			invalidatedBefore := authority.Stats().Invalidated

			file := filepath.Join(root, "written-while-closing.go")
			if err := os.WriteFile(file, []byte("package main\n\nfunc WrittenWhileClosing() {}\n"), 0o644); err != nil {
				t.Fatalf("write file: %v", err)
			}
			if err := idx.IndexFile(file); err != nil {
				t.Fatalf("IndexFile: %v", err)
			}

			if got := authority.Stats().Invalidated; got != invalidatedBefore+1 {
				t.Fatalf("a write admitted under a closed admission must move the witness: invalidated %d -> %d",
					invalidatedBefore, got)
			}
			if err := pin.ValidateCurrent(); !errors.Is(err, graphview.ErrBaseCorpusChanged) {
				t.Fatalf("pin.ValidateCurrent() after a generation-zero write under a closing owner = %v, want ErrBaseCorpusChanged", err)
			}
		})
	}
}

// TestLifecycleTrackedRepositoryIsWitnessedEndToEnd joins the two halves that
// were only ever asserted apart: the LIFECYCLE registers the owner a witness
// needs (CheckoutLifecycle.Register -> trackCheckout -> bindDedicatedGraph ->
// RegisterRepositoryOwner), and the AUTHORITY moves that owner's source when a
// mutation runs. Everything in between is the production code path; the test
// installs the authority exactly as serverstack.NewSharedServer does
// (NewOutputGenerationAuthority(lifecycle.ViewLeases())) and then indexes.
//
// Registering an owner by hand — which every other test here does — proves the
// door works, not that anything opens it.
func TestLifecycleTrackedRepositoryIsWitnessedEndToEnd(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	leases := f.lc.ViewLeases()
	if leases == nil {
		t.Fatal("the lifecycle exposes no lease manager for the authority to witness through")
	}
	authority := NewOutputGenerationAuthority(leases)
	f.mi.SetOutputGenerationAuthority(authority)

	root := f.gitRepo("witnessed-e2e")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if tracked.CatalogErr != nil {
		t.Fatalf("Register catalog: %v", tracked.CatalogErr)
	}

	pin := leases.AcquireBaseCorpus(tracked.Prefix)
	defer pin.Release()
	if !pin.OwnerPinned() {
		t.Fatalf("tracking %q registered no repository owner for a base pin to hold", tracked.Prefix)
	}

	witnessedBefore := authority.Stats().Witnessed
	if _, err := f.mi.IndexAll(); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	if got := authority.Stats().Witnessed; got <= witnessedBefore {
		t.Fatalf("indexing a lifecycle-tracked repository moved no source witness: witnessed %d -> %d",
			witnessedBefore, got)
	}
	if err := pin.ValidateCurrent(); !errors.Is(err, graphview.ErrBaseCorpusChanged) {
		t.Fatalf("base pin after indexing a lifecycle-tracked repository = %v, want ErrBaseCorpusChanged", err)
	}
}
