package graph

import "context"

// ContractInputState belongs to one physical accepted core generation. Ordinary
// edits carry the same identity; only changed contract inputs advance it.
type ContractInputState struct {
	RepoPrefix       string
	CheckoutID       string
	InputVersion     string
	InputFingerprint string
	// Previous fields retain the last published snapshot for checked disjoint scope reuse.
	PreviousInputVersion     string
	PreviousInputFingerprint string
	Accepted                 bool
}

// Cohort reads resolve an explicitly copied generation's carried actor. An
// ambiguous cohort is unavailable; callers never choose an actor's latest row.
type ContractInputStateCohortReader interface {
	ContractInputStatesForRepoContext(context.Context, string) ([]ContractInputState, error)
}

type ContractInputStateReader interface {
	ContractInputStateContext(context.Context, string, string) (ContractInputState, bool, error)
}

// ContractAttachmentKey selects historical analysis exactly. It never means
// the latest attachment for an actor.
type ContractAttachmentKey struct {
	RepoPrefix       string
	CheckoutID       string
	InputVersion     string
	InputFingerprint string
}

// ContractAttachment names a sealed full-repository contract-only payload.
// CompletedTokens is this publication's bounded batch, not lifetime history.
type ContractAttachment struct {
	RepoPrefix        string
	CheckoutID        string
	PayloadGeneration int64
	InputVersion      string
	InputFingerprint  string
	CompletedTokens   []string
}

// Pending readers return bounded unacknowledged work. Implementations
// exclude only exact immutable acknowledgments from available attachments.
type PendingContractWorkReader interface {
	PendingContractWorkForScopeContext(context.Context, string, string) ([]ContractWork, error)
}

type ContractAttachmentReader interface {
	GetContractAttachmentContext(context.Context, ContractAttachmentKey) (*ContractAttachment, error)
}

// Serving completion is bound to an available exact selected attachment; raw
// debt readers never infer completion from a future or sibling publication.
type ContractAttachmentWorkReader interface {
	ContractWorkForAttachmentScopeContext(context.Context, ContractAttachmentKey, string, string) ([]ContractWork, error)
}
