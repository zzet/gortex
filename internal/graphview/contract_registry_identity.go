package graphview

import (
	"context"
	"slices"
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// ContractRegistryIdentity names every input a checked repository registry
// projection through RegistryReader consults: the exact attachment, its payload
// layer's own clocks, and the clocks of each core handle serving owner source
// identities, plus the whole store's mutation clock. Those clocks are
// process-local and monotonic, and each one advances just after its write
// transaction commits. Two equal identities sampled on one live store therefore
// bracket no write to any of these rows whose clock has advanced; a write that
// has committed but not yet advanced its clock can go unseen until it does, the
// same window every cache keyed on these clocks has. It grants no freshness:
// the request still validates its bound views after reading.
type ContractRegistryIdentity struct {
	repo, checkout, version, fingerprint string
	tokens                               []string
	payload                              int64
	layer                                store_sqlite.PayloadInputRevision
	layerEpoch                           uint64
	core                                 bool
	sources                              []contractRegistrySourceIdentity
	store                                uint64
}

type contractRegistrySourceIdentity struct {
	repo     string
	revision store_sqlite.PayloadInputRevision
	epoch    uint64
}

// Equal reports whether both identities name the same projection inputs.
func (id ContractRegistryIdentity) Equal(other ContractRegistryIdentity) bool {
	return id.repo == other.repo && id.checkout == other.checkout && id.version == other.version &&
		id.fingerprint == other.fingerprint && slices.Equal(id.tokens, other.tokens) && id.payload == other.payload &&
		id.layer == other.layer && id.layerEpoch == other.layerEpoch && id.core == other.core &&
		slices.Equal(id.sources, other.sources) && id.store == other.store
}

// RegistryIdentity samples the identity without a database read. It reports
// false when an input has no such identity: a source that is not a plain handle
// on the payload's own store (a composed view, a wrapper or an in-memory
// reader), or a payload layer whose clocks moved after it was opened, whose
// checked projection refuses as stale.
func (v *ContractAnalysisView) RegistryIdentity(ctx context.Context) (ContractRegistryIdentity, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if v == nil || v.Layer == nil || v.Layer.handle == nil {
		return ContractRegistryIdentity{}, false
	}
	reader, ok := v.RegistryReader.(*contractRegistryReader)
	if !ok || reader == nil || reader.layer != v.Layer {
		return ContractRegistryIdentity{}, false
	}
	handle := v.Layer.handle
	layer, err := handle.PayloadInputRevisionContext(ctx)
	if err != nil || layer != v.Layer.inputRevision {
		return ContractRegistryIdentity{}, false
	}
	id := ContractRegistryIdentity{
		repo: v.Attachment.RepoPrefix, checkout: v.Attachment.CheckoutID, version: v.Attachment.InputVersion,
		fingerprint: v.Attachment.InputFingerprint, tokens: slices.Clone(v.Attachment.CompletedTokens),
		payload: v.Attachment.PayloadGeneration, layer: layer, layerEpoch: handle.GenerationCorrectionEpoch(handle.ViewGeneration()),
	}
	sources := reader.sources
	if sources == nil {
		// Without captured sources the core reader serves source identities.
		id.core = true
		sources = map[string]graph.Reader{"": reader.Reader}
	}
	for repo, source := range sources {
		native, ok := source.(*store_sqlite.Store)
		if !ok || native == nil || !native.SharesSymbolSearchCore(handle) {
			return ContractRegistryIdentity{}, false
		}
		revision, err := native.PayloadInputRevisionContext(ctx)
		if err != nil {
			return ContractRegistryIdentity{}, false
		}
		id.sources = append(id.sources, contractRegistrySourceIdentity{repo: repo, revision: revision, epoch: native.GenerationCorrectionEpoch(native.ViewGeneration())})
	}
	sort.Slice(id.sources, func(i, j int) bool { return id.sources[i].repo < id.sources[j].repo })
	id.store = handle.MutationRevision()
	return id, true
}
