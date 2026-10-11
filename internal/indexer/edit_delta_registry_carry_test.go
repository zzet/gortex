package indexer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// The contract registry is carried forward at publication: the registry the
// delta ends with is filed under the published generation's stack, so the
// next chained edit over it is served from memory. At every depth of a chain
// through the overlay fixture's edits the carried registry must equal the
// registry a load of the published view gives, and the next edit must use it.
func TestEditDeltaRegistryIsCarriedForwardExactly(t *testing.T) {
	resetChainOverlayCaches()
	t.Cleanup(resetChainOverlayCaches)
	builderIsolateGit(t)
	store := builderOpenStore(t, "registry-carry")
	repoDir := builderTempDir(t, "checkout-registry-carry")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, chainOverlayTree())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	const commit = int64(1) << 40
	ctx := context.Background()
	render := func(list []contracts.Contract) string {
		lines := make([]string, 0, len(list))
		for _, c := range list {
			data, _ := json.Marshal(c)
			lines = append(lines, string(data))
		}
		sort.Strings(lines)
		return strings.Join(lines, "\n")
	}
	build := func(label string) (int64, *EditDeltaReport, commitLayerBase) {
		t.Helper()
		req := h.request()
		base := commitLayerBase{Reader: store, corpus: store, stack: []int64{commit}}
		if len(h.chain) > 0 {
			manifest, why := loadDirtyChainManifest(ctx, store, h.chain)
			if why != "" {
				t.Fatalf("%s: the chain's manifest: %s", label, why)
			}
			parent := h.chain[len(h.chain)-1]
			base = commitLayerBase{Reader: dirtyChainComposed(t, store, h.chain), corpus: store,
				stack: append([]int64{commit}, h.chain...)}
			req.Identity.BaseGenerationID = parent
			req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(h.chain)
		}
		req.Base = base
		recordLastEditDelta(nil)
		id, report, err := builder.BuildDirtyLayer(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(h.chain) > 0 && report.ParentGenerationID != h.chain[len(h.chain)-1] {
			t.Fatalf("%s: built direct (%q)", label, report.ChainFallbackReason)
		}
		h.chain = append(h.chain, id)
		return id, LastEditDeltaReport(), base
	}
	contractsSeen := 0
	var viewDiffers []string
	edits := chainOverlayEdits()
	for i, edit := range edits {
		for rel, body := range edit.write {
			if rel == "go.mod" {
				continue // a manifest change builds direct; the chain is what is tested
			}
			full := filepath.Join(repoDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			builderWriteFile(t, repoDir, rel, body)
		}
		for _, rel := range edit.drop {
			if err := os.Remove(filepath.Join(repoDir, filepath.FromSlash(rel))); err != nil {
				t.Fatal(err)
			}
		}
		_, delta, _ := build(edit.name)
		if i > 0 && !delta.ContractRegistryCached {
			t.Fatalf("%s: the chained edit reloaded the registry instead of the one carried to its stack", edit.name)
		}
		// The registry filed under the new stack equals a load of the
		// published view.
		next := commitLayerBase{Reader: dirtyChainComposed(t, store, h.chain), corpus: store,
			stack: append([]int64{commit}, h.chain...)}
		key, ok := editDeltaRegistryKey(next, store, builderRepoPrefix, builderRepoPrefix, builderRepoPrefix)
		if !ok {
			t.Fatalf("%s: no registry key for the published stack", edit.name)
		}
		var carried []contracts.Contract
		found := false
		editDeltaContractCache.Lock()
		for _, entry := range editDeltaContractCache.entries {
			if entry.key == key {
				carried, found = entry.contracts, true
			}
		}
		editDeltaContractCache.Unlock()
		if !found {
			t.Fatalf("%s: nothing was carried to the published stack", edit.name)
		}
		loaded := contracts.LoadRegistryFromGraphWithScope(graph.NewDeltaWriter(next.Reader, nil), builderRepoPrefix, builderRepoPrefix, builderRepoPrefix)
		var fresh []contracts.Contract
		if loaded != nil {
			fresh = loaded.ByRepo(builderRepoPrefix)
		}
		contractsSeen += len(fresh)
		// The oracle is a clean whole index of the working tree; the
		// published view (what an uncached delta reads) must hold the same
		// records.
		clean := builderOpenStore(t, "registry-clean-"+edit.name)
		builderIndex(t, clean, repoDir)
		var want []contracts.Contract
		if reg := contracts.LoadRegistryFromGraphWithScope(clean, builderRepoPrefix, builderRepoPrefix, builderRepoPrefix); reg != nil {
			want = reg.ByRepo(builderRepoPrefix)
		}
		// A whole index and the per-file path stamp different contract
		// metadata (the handler's trail), so the clean index is compared by
		// record identity; where the published view holds the clean index's
		// records, the carried registry must equal its load in full.
		if got, exp := renderIdentity(carried), renderIdentity(want); got != exp {
			t.Fatalf("%s: the carried registry's records differ from a clean index's\ncarried:\n%s\nclean:\n%s", edit.name, got, exp)
		}
		if renderIdentity(fresh) != renderIdentity(want) {
			viewDiffers = append(viewDiffers, edit.name)
		} else if got, exp := render(carried), render(fresh); got != exp {
			t.Fatalf("%s: the carried registry differs from a load of the published view\ncarried:\n%s\nloaded:\n%s", edit.name, got, exp)
		}
	}
	if len(viewDiffers) > 0 {
		t.Fatalf("the published view loads a registry other than the clean index's at %v", viewDiffers)
	}
	if contractsSeen == 0 {
		t.Fatal("fixture precondition: the chain carried no contract at all")
	}
	t.Logf("contracts compared over %d depths: %d", len(edits), contractsSeen)
}

// renderIdentity renders a registry by record identity: ID, type, role, file
// and line. The symbol of a record whose ID two files share is the registry's
// pick among them, which differs between two loads of the same graph (a
// clean index included), so it is not part of the identity.
func renderIdentity(list []contracts.Contract) string {
	lines := make([]string, 0, len(list))
	for _, c := range list {
		lines = append(lines, strings.Join([]string{c.ID, string(c.Type), string(c.Role), c.FilePath, strconv.Itoa(c.Line)}, "|"))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
