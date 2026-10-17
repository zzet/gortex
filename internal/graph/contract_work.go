package graph

import "context"

// ContractWork identifies one accepted set of inputs whose contract analysis
// has not yet landed. Tokens identify work, not paths: deleting or editing the
// same path again must not let an older worker acknowledge newer inputs.
type ContractWork struct {
	Token            string
	OriginGeneration int64
	CheckoutID       string
	RepoPrefix       string
	FilePath         string
	InputVersion     string
	InputFingerprint string
	State            ContractWorkState
	Scope            ContractWorkScope
}

type ContractWorkState string

const (
	ContractWorkPending  ContractWorkState = "pending"
	ContractWorkComplete ContractWorkState = "complete"
)

// Scope retains the old as well as the new frontier. A deleted source can owe
// analysis of surviving owners and consumers even though it owns no rows.
type ContractWorkScope struct {
	Groups     []ContractWorkGroup `json:"groups,omitempty"`
	SymbolIDs  []string            `json:"symbol_ids,omitempty"`
	LookupKeys []string            `json:"lookup_keys,omitempty"`
	Causes     []string            `json:"causes,omitempty"`
	Deleted    bool                `json:"deleted,omitempty"`
	Unknown    bool                `json:"unknown,omitempty"`
}

type ContractWorkGroup struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	ContractID  string `json:"contract_id"`
}

// Readers return physical work rows with errors rather than interpreting a
// failed read as completed analysis. Composition applies layers bottom-up;
// the last row for a token wins. A different token is independent work.
type ContractWorkReader interface {
	ContractWorkContext(context.Context) ([]ContractWork, error)
}
