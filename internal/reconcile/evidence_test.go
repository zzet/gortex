package reconcile

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Volume tokens used across the evidence table. Two distinct tokens is all the
// classifier needs: same token means the same mounted volume, different means
// the root's volume is not the one the surviving ancestor sits on.
const (
	volumeA = "dev-A"
	volumeB = "dev-B"
)

// sampleOnVolume is a fresh sample of a root that still exists on token.
func sampleOnVolume(token string) PathEvidence {
	return PathEvidence{
		RootExists:          true,
		RootIdentity:        gitstate.VolumeKindUnixDev + ":" + token + ":42",
		RootVolumeKind:      gitstate.VolumeKindUnixDev,
		RootVolumeToken:     token,
		AncestorPath:        "/parent",
		AncestorVolumeKind:  gitstate.VolumeKindUnixDev,
		AncestorVolumeToken: token,
	}
}

// sampleAbsentAncestorOn is a fresh sample of a vanished root whose nearest
// surviving ancestor sits on token.
func sampleAbsentAncestorOn(token string) PathEvidence {
	return PathEvidence{
		AncestorPath:        "/parent",
		AncestorVolumeKind:  gitstate.VolumeKindUnixDev,
		AncestorVolumeToken: token,
	}
}

// listedRecord is a worktree git still administers whose root is unreachable
// for the given reason.
func listedRecord(prunable bool, rootErr error) *gitstate.WorktreeRecord {
	return &gitstate.WorktreeRecord{
		Path:           "/repo/wt",
		AdminName:      "wt",
		Prunable:       prunable,
		RootAccessible: false,
		RootErr:        rootErr,
	}
}

func TestClassifyEvidenceMatrix(t *testing.T) {
	inventoryDown := fmt.Errorf("git refused: %w", gitstate.ErrInventoryUnavailable)
	mismatch := fmt.Errorf("%w: other family", ErrCommonDirMismatch)
	accessible := &gitstate.WorktreeRecord{Path: "/repo/wt", AdminName: "wt", RootAccessible: true}

	// The persisted sample every prunable case is compared against: the root
	// was there, on volume A, with a usable ancestor.
	persistedOnA := sampleOnVolume(volumeA)

	for _, tc := range []struct {
		name         string
		inventoryErr error
		record       *gitstate.WorktreeRecord
		persisted    PathEvidence
		fresh        PathEvidence
		want         Disposition
		wantEvidence RemovalEvidence
	}{
		{
			name:         "inventory unavailable with a listed accessible record",
			inventoryErr: inventoryDown, record: accessible,
			persisted: persistedOnA, fresh: sampleOnVolume(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:         "inventory unavailable with no record at all",
			inventoryErr: inventoryDown, record: nil,
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:         "inventory unavailable outranks a fully confirmed prune",
			inventoryErr: inventoryDown, record: listedRecord(true, fs.ErrNotExist),
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:         "inventory came from another family's common dir",
			inventoryErr: mismatch, record: nil,
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "trustworthy inventory omits a known admin name",
			record:    nil,
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionRemoved, wantEvidence: EvidenceAuthoritativeOmission,
		},
		{
			name:      "listed record with a reachable root",
			record:    accessible,
			persisted: persistedOnA, fresh: sampleOnVolume(volumeA),
			want: DispositionPresent, wantEvidence: EvidenceNone,
		},
		{
			name:      "unreachable root git does not call prunable",
			record:    listedRecord(false, fs.ErrNotExist),
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable but the root lstat was a permission error",
			record:    listedRecord(true, fs.ErrPermission),
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable but the root lstat was an io error",
			record:    listedRecord(true, errors.New("input/output error")),
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable but no stat error was recorded",
			record:    listedRecord(true, nil),
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable but the fresh sample found the root back",
			record:    listedRecord(true, fs.ErrNotExist),
			persisted: persistedOnA, fresh: sampleOnVolume(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable with no persisted evidence at all",
			record:    listedRecord(true, fs.ErrNotExist),
			persisted: PathEvidence{}, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:   "prunable with an unsupported persisted root volume kind",
			record: listedRecord(true, fs.ErrNotExist),
			persisted: PathEvidence{
				RootExists: true, RootVolumeKind: gitstate.VolumeKindUnsupported, RootVolumeToken: volumeA,
				AncestorPath: "/parent", AncestorVolumeKind: gitstate.VolumeKindUnixDev, AncestorVolumeToken: volumeA,
			},
			fresh: sampleAbsentAncestorOn(volumeA),
			want:  DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:   "prunable with an empty persisted root volume token",
			record: listedRecord(true, fs.ErrNotExist),
			persisted: PathEvidence{
				RootExists: true, RootVolumeKind: gitstate.VolumeKindUnixDev,
				AncestorPath: "/parent", AncestorVolumeKind: gitstate.VolumeKindUnixDev, AncestorVolumeToken: volumeA,
			},
			fresh: sampleAbsentAncestorOn(volumeA),
			want:  DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:   "prunable with no persisted ancestor evidence",
			record: listedRecord(true, fs.ErrNotExist),
			persisted: PathEvidence{
				RootExists: true, RootVolumeKind: gitstate.VolumeKindUnixDev, RootVolumeToken: volumeA,
			},
			fresh: sampleAbsentAncestorOn(volumeA),
			want:  DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable but no fresh ancestor could be reached",
			record:    listedRecord(true, fs.ErrNotExist),
			persisted: persistedOnA, fresh: PathEvidence{},
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable but the fresh ancestor volume kind is unsupported",
			record:    listedRecord(true, fs.ErrNotExist),
			persisted: persistedOnA,
			fresh: PathEvidence{
				AncestorPath: "/parent", AncestorVolumeKind: gitstate.VolumeKindUnsupported, AncestorVolumeToken: volumeA,
			},
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable but the surviving ancestor is on another volume",
			record:    listedRecord(true, fs.ErrNotExist),
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeB),
			want: DispositionInaccessible, wantEvidence: EvidenceNone,
		},
		{
			name:      "prunable, absent, and the volume is still mounted",
			record:    listedRecord(true, fs.ErrNotExist),
			persisted: persistedOnA, fresh: sampleAbsentAncestorOn(volumeA),
			want: DispositionRemoved, wantEvidence: EvidencePrunableConfirmed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.inventoryErr, tc.record, tc.persisted, tc.fresh)
			if got.Disposition != tc.want {
				t.Errorf("Disposition = %q, want %q (detail: %s)", got.Disposition, tc.want, got.Detail)
			}
			if got.Evidence != tc.wantEvidence {
				t.Errorf("Evidence = %q, want %q", got.Evidence, tc.wantEvidence)
			}
			if got.Detail == "" {
				t.Error("Detail is empty; every verdict must say which branch decided it")
			}
			wantCode := ""
			if tc.want == DispositionInaccessible {
				wantCode = graphview.CodeCheckoutInaccessible
			}
			if got.Code != wantCode {
				t.Errorf("Code = %q, want %q", got.Code, wantCode)
			}
		})
	}
}

func TestValidateInventory(t *testing.T) {
	inv := &gitstate.FamilyInventory{CommonDir: "/repo/.git"}
	sampleErr := fmt.Errorf("boom: %w", gitstate.ErrInventoryUnavailable)

	if err := ValidateInventory(inv, nil, "/repo/.git"); err != nil {
		t.Errorf("matching common dir = %v, want nil", err)
	}
	if err := ValidateInventory(inv, sampleErr, "/repo/.git"); !errors.Is(err, gitstate.ErrInventoryUnavailable) {
		t.Errorf("carried error = %v, want it wrapped", err)
	}
	if err := ValidateInventory(nil, nil, "/repo/.git"); !errors.Is(err, gitstate.ErrInventoryUnavailable) {
		t.Errorf("nil inventory = %v, want unavailable", err)
	}
	err := ValidateInventory(inv, nil, "/other/.git")
	if !errors.Is(err, ErrCommonDirMismatch) {
		t.Errorf("mismatch = %v, want ErrCommonDirMismatch", err)
	}
	// A mismatch must reach Classify as an inventory failure, or a family
	// reconciled against a foreign inventory would read every record as gone.
	if !errors.Is(err, gitstate.ErrInventoryUnavailable) {
		t.Errorf("mismatch = %v, want it to also be an inventory failure", err)
	}
	if err := ValidateInventory(inv, nil, ""); !errors.Is(err, ErrCommonDirMismatch) {
		t.Errorf("unrecorded common dir = %v, want ErrCommonDirMismatch", err)
	}
}

func TestPathEvidenceLowering(t *testing.T) {
	sample := gitstate.PathEvidence{
		RootExists:          true,
		RootIdentity:        "unix-dev:7:99",
		VolumeKind:          gitstate.VolumeKindUnixDev,
		VolumeToken:         "7",
		AncestorPath:        "/parent",
		AncestorVolumeKind:  gitstate.VolumeKindUnixDev,
		AncestorVolumeToken: "7",
	}
	lowered := SampledPathEvidence(sample)
	if !lowered.RootExists || lowered.RootVolumeToken != "7" || lowered.AncestorPath != "/parent" {
		t.Fatalf("SampledPathEvidence = %+v", lowered)
	}
	if !lowered.rootVolumeUsable() || !lowered.ancestorVolumeUsable() {
		t.Fatal("a complete unix sample must be usable on both axes")
	}

	row := lowered.CatalogRow("wt-1", 1234, 5)
	if row.CheckoutID != "wt-1" || row.SampledAt != 1234 || row.SampleGeneration != 5 {
		t.Fatalf("CatalogRow = %+v", row)
	}
	round := StoredPathEvidence(row)
	if round != lowered {
		t.Fatalf("round trip = %+v, want %+v", round, lowered)
	}

	// A row that never recorded a root identity must not read back as a root
	// that existed, or a never-sampled checkout would look confirmed.
	empty := StoredPathEvidence(store_sqlite.CheckoutPathEvidence{CheckoutID: "wt-2"})
	if empty.RootExists || empty.rootVolumeUsable() || empty.ancestorVolumeUsable() {
		t.Fatalf("empty row lowered to %+v", empty)
	}

	unsupported := SampledPathEvidence(gitstate.PathEvidence{
		RootExists: true, VolumeKind: gitstate.VolumeKindUnsupported,
		AncestorPath: "/parent", AncestorVolumeKind: gitstate.VolumeKindUnsupported,
	})
	if unsupported.rootVolumeUsable() || unsupported.ancestorVolumeUsable() {
		t.Fatal("an unsupported-platform sample must never count as usable evidence")
	}
}
