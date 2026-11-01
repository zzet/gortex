//go:build linux || darwin

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

// This is only a byte-copy/parser fixture, deliberately not a real SQLite
// database or upgrade receipt. It never establishes upgrade acceptance.
func syntheticDiscoverySeed(t *testing.T) (runOptions, []Corpus, discoverySeedLineage) {
	t.Helper()
	first, second := gitFixture(t), gitFixture(t)
	second.Label = "other"
	scratch := t.TempDir()
	descriptor := filepath.Join(scratch, "prepared.json")
	o := runOptions{Binary: "unused", Scratch: scratch, Discovery: discoveryOptions{Prepare: descriptor, ExistingWorktrees: 2, ExistingNested: 1}}
	corpora := []Corpus{first, second}
	require.NoError(t, prepareDiscoveryState(t.Context(), o, corpora))
	var d discoverySeedDescriptor
	require.NoError(t, discoveryReadJSON(descriptor, &d))
	snapshot := filepath.Join(d.Root, "synthetic-byte-snapshot.db")
	header := make([]byte, 100)
	copy(header, "SQLite format 3\x00")
	binary.BigEndian.PutUint32(header[60:64], 44)
	require.NoError(t, os.WriteFile(snapshot, header, 0o600))
	mainSHA, err := discoveryFileSHA(snapshot)
	require.NoError(t, err)
	child := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, child.Run())
	l := discoverySeedLineage{State: "VALIDATED_OWNED_UPGRADE", Descriptor: descriptor, OwnedRoot: d.Root, SourceGeneration: "synthetic-test-only", OldVersion: "synthetic", OldBinarySHA256: strings.Repeat("1", 64), UpgradeSourceCommit: strings.Repeat("2", 40), UpgradeBinarySHA256: strings.Repeat("3", 64), Snapshot: snapshot, MainSHA256: mainSHA, SchemaVersion: 44, NormalStop: true, OwnersExited: true, OwnerPID: child.Process.Pid, StopReceipt: filepath.Join(d.Root, "synthetic-stop.json"), IdentityReceipt: filepath.Join(d.Root, "synthetic-identity.json"), Repos: d.Repos}
	l.DescriptorSHA256, err = discoveryFileSHA(descriptor)
	require.NoError(t, err)
	rewriteSyntheticDiscoveryProofs(t, &l)
	o.Discovery = discoveryOptions{Resume: filepath.Join(scratch, "lineage.json")}
	return o, corpora, l
}
func rewriteSyntheticDiscoveryProofs(t *testing.T, l *discoverySeedLineage) {
	t.Helper()
	catalogPath := filepath.Join(l.OwnedRoot, "synthetic-catalog.json")
	var scopes []discoveryCatalogScope
	for i := 0; i < 2; i++ {
		repo := l.Repos[i]
		family := indexer.FamilyOverview{FamilyID: fmt.Sprintf("synthetic-family-%d", i), CommonDir: repo.CommonDir, PrimaryRepoPrefix: repo.Repo.Prefix}
		family.Graphs = []indexer.GraphOverview{{GraphID: fmt.Sprintf("synthetic-graph-%d", i), RepoPrefix: repo.Repo.Prefix, OwnerCheckoutID: fmt.Sprintf("synthetic-checkout-%d", i)}}
		for j, row := range l.Repos {
			if row.CommonDir != repo.CommonDir {
				continue
			}
			checkout := indexer.CheckoutOverview{CheckoutID: fmt.Sprintf("synthetic-checkout-%d", j), Incarnation: fmt.Sprintf("synthetic-incarnation-%d", j), RootPath: row.Repo.Path}
			if j == i {
				checkout.GraphID = family.Graphs[0].GraphID
			}
			family.Checkouts = append(family.Checkouts, checkout)
		}
		scopes = append(scopes, discoveryCatalogScope{CWD: repo.Repo.Path, Overview: indexer.FamiliesOverview{Families: []indexer.FamilyOverview{family}}})
	}
	_ = os.Remove(catalogPath)
	require.NoError(t, discoveryWriteJSON(catalogPath, discoveryCatalogReceipt{Before: scopes, After: scopes}))
	catalogSHA, err := discoveryFileSHA(catalogPath)
	require.NoError(t, err)
	proof := discoverySeedProof{CatalogReceipt: catalogPath, CatalogReceiptSHA256: catalogSHA, State: "OBSERVED_OWNED_UPGRADE", ObservedAt: time.Now(), OwnedRoot: l.OwnedRoot, OwnerPID: l.OwnerPID, NormalStop: l.NormalStop, OwnersExited: l.OwnersExited, MainSHA256: l.MainSHA256, WALSHA256: l.WALSHA256, SchemaVersion: l.SchemaVersion, UpgradeBinarySHA256: l.UpgradeBinarySHA256, Repos: l.Repos}
	for _, path := range []string{l.StopReceipt, l.IdentityReceipt} {
		_ = os.Remove(path)
		require.NoError(t, discoveryWriteJSON(path, proof))
	}
	l.StopReceiptSHA256, err = discoveryFileSHA(l.StopReceipt)
	require.NoError(t, err)
	l.IdentityReceiptSHA256, err = discoveryFileSHA(l.IdentityReceipt)
	require.NoError(t, err)
}
func TestDiscoverySeedResumeRefusesUnknownLiveAndMismatchedLineage(t *testing.T) {
	for _, name := range []string{"unknown", "topology", "snapshot", "live", "shared_memory", "changed_git_family"} {
		t.Run(name, func(t *testing.T) {
			o, corpora, l := syntheticDiscoverySeed(t)
			switch name {
			case "unknown":
				l.State = "TEMPLATE"
			case "topology":
				l.Repos = append([]discoverySeedRepo(nil), l.Repos...)
				l.Repos[2].Repo.Prefix = "foreign"
			case "snapshot":
				l.MainSHA256 = strings.Repeat("0", 64)
				rewriteSyntheticDiscoveryProofs(t, &l)
			case "live":
				l.OwnerPID = os.Getpid()
				rewriteSyntheticDiscoveryProofs(t, &l)
			case "shared_memory":
				require.NoError(t, os.WriteFile(l.Snapshot+"-shm", nil, 0o600))
			case "changed_git_family":
				require.NoError(t, os.WriteFile(filepath.Join(l.Repos[2].Repo.Path, ".git"), []byte("gitdir: "+filepath.Join(l.Repos[1].Repo.Path, ".git")+"\n"), 0o600))
			}
			require.NoError(t, discoveryWriteJSON(o.Discovery.Resume, l))
			_, err := resumeDiscoveryState(t.Context(), o, corpora)
			require.Error(t, err)
			require.NoFileExists(t, filepath.Join(l.OwnedRoot, "store.db"), "refused lineage must not create a destination store")
		})
	}
}
func TestDiscoverySeedCopiesSnapshotWithoutChangingExistingIdentities(t *testing.T) {
	o, corpora, l := syntheticDiscoverySeed(t)
	require.NoError(t, discoveryWriteJSON(o.Discovery.Resume, l))
	p, err := resumeDiscoveryState(t.Context(), o, corpora)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir) })
	require.Equal(t, l.OwnedRoot, p.Root)
	require.Len(t, p.Repos, 4)
	require.True(t, inside(p.Repos[0].Path, p.Repos[2].Path))
	require.False(t, inside(p.Repos[0].Path, p.Repos[3].Path))
	for i, repo := range p.Repos {
		require.Equal(t, l.Repos[i].Repo.Path, repo.Path)
		require.Equal(t, l.Repos[i].Repo.Prefix, repo.Prefix)
	}
	source, err := discoveryFileSHA(l.Snapshot)
	require.NoError(t, err)
	dest, err := discoveryFileSHA(p.Store)
	require.NoError(t, err)
	require.Equal(t, l.MainSHA256, source)
	require.Equal(t, source, dest)
	_, err = resumeDiscoveryState(t.Context(), o, corpora)
	require.Error(t, err, "a prepared run is consumed once")
}

func TestDiscoverySeedRefusesRekeyedCatalogBeforeCopy(t *testing.T) {
	o, corpora, l := syntheticDiscoverySeed(t)
	var proof discoverySeedProof
	require.NoError(t, discoveryReadJSON(l.IdentityReceipt, &proof))
	var raw discoveryCatalogReceipt
	require.NoError(t, discoveryReadJSON(proof.CatalogReceipt, &raw))
	raw.After[0].Overview.Families[0].Checkouts[1].Incarnation = "synthetic-rekeyed-incarnation"
	require.NoError(t, os.Remove(proof.CatalogReceipt))
	require.NoError(t, discoveryWriteJSON(proof.CatalogReceipt, raw))
	var err error
	proof.CatalogReceiptSHA256, err = discoveryFileSHA(proof.CatalogReceipt)
	require.NoError(t, err)
	for _, path := range []string{l.StopReceipt, l.IdentityReceipt} {
		require.NoError(t, os.Remove(path))
		require.NoError(t, discoveryWriteJSON(path, proof))
	}
	l.StopReceiptSHA256, err = discoveryFileSHA(l.StopReceipt)
	require.NoError(t, err)
	l.IdentityReceiptSHA256, err = discoveryFileSHA(l.IdentityReceipt)
	require.NoError(t, err)
	require.NoError(t, discoveryWriteJSON(o.Discovery.Resume, l))
	_, err = resumeDiscoveryState(t.Context(), o, corpora)
	require.ErrorContains(t, err, "catalog identity mapping changed")
	require.NoFileExists(t, filepath.Join(l.OwnedRoot, "store.db"))
}
