package graph

import "context"

// ContractBoundaryReceipt is a file-owned parser result. Payload is opaque to
// storage; keys drive indexed dependency lookup even when no records exist.
// Staging retains one accepted predecessor and the union of all in-flight key
// memberships until exact outer core acceptance replaces them.
type ContractBoundaryReceipt struct {
	RepoPrefix            string
	CheckoutID            string
	FilePath              string
	Version               string
	Fingerprint           string
	SourceFingerprint     string
	Accepted              bool
	Deleted               bool
	Payload               []byte
	LookupKeys            []string
	ProducedKeys          []string
	Scope                 ContractWorkScope
	Carried               bool
	CarriedFromGeneration int64
	CarriedFromCheckoutID string
	Previous              *ContractBoundaryReceipt
}

// A complete namespace baseline certifies absence for newly created files.
// Missing/legacy baselines remain unknown rather than inventing an empty receipt.
type ContractBoundaryReceiptBaseline struct {
	RepoPrefix  string
	CheckoutID  string
	Version     string
	Fingerprint string
}

type ContractBoundaryReceiptReader interface {
	ContractBoundaryReceiptContext(context.Context, string, string, string) (*ContractBoundaryReceipt, bool, error)
	ContractBoundaryReceiptForVersionContext(context.Context, string, string, string, string) (*ContractBoundaryReceipt, bool, error)
	ContractBoundaryReceiptBaselineContext(context.Context, string, string) (*ContractBoundaryReceiptBaseline, error)
	ContractBoundaryReceiptsForLookupKeysContext(context.Context, []string, int) ([]ContractBoundaryReceipt, error)
	ContractBoundaryReceiptsForProducedKeysContext(context.Context, []string, int) ([]ContractBoundaryReceipt, error)
}

// ContractBoundaryReceiptSource is an exact leased selected physical capture.
// TargetCheckoutID explicitly retags copied receipt ownership; carried origin
// remains recorded in the blob. Generation zero requires trusted actual
// inheritance selection by the caller, not inferred catalog ancestry.
type ContractBoundaryReceiptSource struct {
	GenerationID     int64
	Receipt          ContractBoundaryReceipt
	TargetCheckoutID string
}
