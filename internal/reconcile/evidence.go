package reconcile

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// RemovalEvidence names what justified concluding that a checkout is gone.
// Nothing is ever deleted on a hunch: a removal carries the evidence that
// produced it, the evidence is written to the catalog beside the removal
// clock, and a reader of the row can tell afterwards which of the two very
// different proofs was used.
type RemovalEvidence string

const (
	// EvidenceAuthoritativeOmission means a trustworthy inventory of the
	// family did not list the checkout. Git's administrative data is the
	// authority on which worktrees exist, so its silence is a statement.
	EvidenceAuthoritativeOmission RemovalEvidence = "evidence_authoritative_omission"
	// EvidencePrunableConfirmed means git still lists the worktree but calls
	// it prunable, and the filesystem agrees the root is really absent from a
	// volume that is still mounted.
	EvidencePrunableConfirmed RemovalEvidence = "evidence_prunable_confirmed"
	// EvidenceNone is the value carried by every conclusion that is not a
	// removal. It is a real value rather than the empty string so an evidence
	// field is never ambiguous between "not a removal" and "not filled in".
	EvidenceNone RemovalEvidence = "evidence_none"
)

// Disposition is what one reconciliation concluded about a checkout.
type Disposition string

const (
	// DispositionPresent means the checkout answered.
	DispositionPresent Disposition = "present"
	// DispositionInaccessible means the checkout did not answer and nothing
	// proves it is gone. It is the default for every uncertain case.
	DispositionInaccessible Disposition = "inaccessible"
	// DispositionRemoved means the checkout is gone, with evidence.
	DispositionRemoved Disposition = "removed"
)

// Classification is one reconciliation's verdict on one checkout.
type Classification struct {
	// Disposition is the verdict.
	Disposition Disposition
	// Evidence is what justified a removal, EvidenceNone otherwise.
	Evidence RemovalEvidence
	// Code is the stable wire code for verdicts that have one, empty
	// otherwise. Inaccessibility reuses graphview.CodeCheckoutInaccessible
	// rather than minting a parallel spelling of the same condition.
	Code string
	// Detail is a short technical statement of which branch decided the
	// verdict. It is for humans reading a report; nothing switches on it.
	Detail string
}

// PathEvidence is one filesystem sample of a checkout root, in the shape the
// classifier compares.
//
// The persisted sample (a catalog row) and the fresh sample (a gitstate read)
// are both lowered into this one type on purpose: a volume comparison between
// two differently-shaped structs is exactly the place a field would silently
// be read from the wrong side.
type PathEvidence struct {
	// RootExists is true when the root itself was reachable at sample time.
	RootExists bool
	// RootIdentity is the opaque token of the root object, empty when the
	// root did not exist or the platform reports no identity.
	RootIdentity string
	// RootVolumeKind names how RootVolumeToken was derived.
	RootVolumeKind string
	// RootVolumeToken identifies the volume the root sat on.
	RootVolumeToken string
	// AncestorPath is the nearest existing strict ancestor of the root.
	AncestorPath string
	// AncestorVolumeKind names how AncestorVolumeToken was derived.
	AncestorVolumeKind string
	// AncestorVolumeToken identifies the volume that ancestor sits on.
	AncestorVolumeToken string
}

// SampledPathEvidence lowers a fresh gitstate sample.
func SampledPathEvidence(sample gitstate.PathEvidence) PathEvidence {
	return PathEvidence{
		RootExists:          sample.RootExists,
		RootIdentity:        sample.RootIdentity,
		RootVolumeKind:      sample.VolumeKind,
		RootVolumeToken:     sample.VolumeToken,
		AncestorPath:        sample.AncestorPath,
		AncestorVolumeKind:  sample.AncestorVolumeKind,
		AncestorVolumeToken: sample.AncestorVolumeToken,
	}
}

// StoredPathEvidence lowers a persisted catalog row.
//
// The stored row has no "did the root exist" column. A root identity is only
// ever written for a root that was statted, so its presence is what records
// that the root was there when the sample was taken.
func StoredPathEvidence(row store_sqlite.CheckoutPathEvidence) PathEvidence {
	return PathEvidence{
		RootExists:          row.RootPathIdentity != "",
		RootIdentity:        row.RootPathIdentity,
		RootVolumeKind:      row.RootVolumeKind,
		RootVolumeToken:     row.RootVolumeToken,
		AncestorPath:        row.NearestExistingAncestorPath,
		AncestorVolumeKind:  row.AncestorVolumeKind,
		AncestorVolumeToken: row.AncestorVolumeToken,
	}
}

// CatalogRow raises evidence back into the row shape the catalog stores for
// one checkout. The common-dir columns are left to whoever samples the shared
// git directory; a checkout-root sample knows nothing about it.
func (e PathEvidence) CatalogRow(checkoutID string, sampledAt, generation int64) store_sqlite.CheckoutPathEvidence {
	return store_sqlite.CheckoutPathEvidence{
		CheckoutID:                  checkoutID,
		RootPathIdentity:            e.RootIdentity,
		RootVolumeKind:              e.RootVolumeKind,
		RootVolumeToken:             e.RootVolumeToken,
		NearestExistingAncestorPath: e.AncestorPath,
		AncestorVolumeKind:          e.AncestorVolumeKind,
		AncestorVolumeToken:         e.AncestorVolumeToken,
		SampledAt:                   sampledAt,
		SampleGeneration:            generation,
	}
}

// rootVolumeUsable reports whether the sample identifies the volume the root
// sat on well enough to compare with another sample.
func (e PathEvidence) rootVolumeUsable() bool {
	return e.RootVolumeToken != "" && volumeKindSupported(e.RootVolumeKind)
}

// ancestorVolumeUsable reports whether the sample reached an ancestor and read
// a comparable volume token off it.
func (e PathEvidence) ancestorVolumeUsable() bool {
	return e.AncestorPath != "" && e.AncestorVolumeToken != "" && volumeKindSupported(e.AncestorVolumeKind)
}

// volumeKindSupported rejects the kinds that carry no information: the empty
// kind of a sample that found nothing, and the explicit "this platform does
// not expose volume identity" kind. Comparing two empty tokens would otherwise
// read as "same volume".
func volumeKindSupported(kind string) bool {
	return kind != "" && kind != gitstate.VolumeKindUnsupported
}

// ErrCommonDirMismatch reports that an inventory describes some other checkout
// family than the one being reconciled. It wraps gitstate.ErrInventoryUnavailable
// because that is exactly what it means for this family: the inventory in hand
// says nothing about it, so a missing record is not a removal.
var ErrCommonDirMismatch = fmt.Errorf(
	"reconcile: inventory does not come from the family's recorded common dir: %w",
	gitstate.ErrInventoryUnavailable)

// ValidateInventory reports whether inv may be read as the authority on the
// family whose recorded common-dir identity is familyCommonDir.
//
// A nil return is the caller's licence to treat a missing record as a removal.
// Any other return must be passed to Classify as the inventory error, which
// makes every checkout of the family inaccessible instead.
func ValidateInventory(inv *gitstate.FamilyInventory, invErr error, familyCommonDir string) error {
	if invErr != nil {
		return invErr
	}
	if inv == nil {
		return fmt.Errorf("reconcile: no inventory was produced: %w", gitstate.ErrInventoryUnavailable)
	}
	if familyCommonDir == "" || inv.CommonDir != familyCommonDir {
		return fmt.Errorf("%w: inventory reports %q, family records %q",
			ErrCommonDirMismatch, inv.CommonDir, familyCommonDir)
	}
	return nil
}

// Classify decides what one checkout's observation means.
//
// The asymmetry is deliberate: presence and inaccessibility are cheap to be
// wrong about, removal is not, so every path that is not positively proven
// lands on inaccessible. inventoryErr non-nil short-circuits everything —
// without a trustworthy inventory, an absent record is an absent inventory,
// not an absent checkout.
//
// record is nil when a usable inventory did not list the checkout's admin
// name. persisted is the last sample stored for the checkout; fresh is a
// sample taken during this pass.
func Classify(inventoryErr error, record *gitstate.WorktreeRecord, persisted, fresh PathEvidence) Classification {
	if inventoryErr != nil {
		return Classification{
			Disposition: DispositionInaccessible,
			Evidence:    EvidenceNone,
			Code:        graphview.CodeCheckoutInaccessible,
			Detail:      "inventory unavailable: " + inventoryErr.Error(),
		}
	}
	if record == nil {
		return Classification{
			Disposition: DispositionRemoved,
			Evidence:    EvidenceAuthoritativeOmission,
			Detail:      "the family's own inventory does not list this admin name",
		}
	}
	if record.RootAccessible {
		return Classification{
			Disposition: DispositionPresent,
			Evidence:    EvidenceNone,
			Detail:      "git lists the worktree and its root is reachable",
		}
	}
	detail, confirmed := prunableConfirmed(record, persisted, fresh)
	if confirmed {
		return Classification{
			Disposition: DispositionRemoved,
			Evidence:    EvidencePrunableConfirmed,
			Detail:      detail,
		}
	}
	return Classification{
		Disposition: DispositionInaccessible,
		Evidence:    EvidenceNone,
		Code:        graphview.CodeCheckoutInaccessible,
		Detail:      detail,
	}
}

// prunableConfirmed reports whether a listed-but-unreachable worktree is
// provably deleted, and states which condition decided it either way.
//
// Every condition has to hold. The volume comparison is what separates "the
// directory was deleted" from "the disk it lived on is not mounted": the root
// is gone, but the nearest directory that still exists above it sits on the
// same volume the root used to sit on, so that volume is mounted and the
// absence is a real absence.
func prunableConfirmed(record *gitstate.WorktreeRecord, persisted, fresh PathEvidence) (string, bool) {
	switch {
	case !record.Prunable:
		return "root is unreachable but git does not consider the worktree prunable", false
	case !errors.Is(record.RootErr, fs.ErrNotExist):
		return "root lstat did not report an absence: " + errText(record.RootErr), false
	case fresh.RootExists:
		return "the fresh sample found the root back in place", false
	case !persisted.rootVolumeUsable():
		return "no usable persisted root volume token to compare a fresh sample against", false
	case !persisted.ancestorVolumeUsable():
		return "the persisted sample carries no usable ancestor volume token", false
	case !fresh.ancestorVolumeUsable():
		return "no reachable ancestor to read a volume token from", false
	case fresh.AncestorVolumeToken != persisted.RootVolumeToken:
		return "the nearest existing ancestor is on a different volume than the root was", false
	}
	return "git reports the worktree prunable and the root is absent from a still-mounted volume", true
}

// errText renders an error for a detail string, naming the nil case rather
// than printing "%!v(nil)".
func errText(err error) string {
	if err == nil {
		return "no error was recorded"
	}
	return err.Error()
}
