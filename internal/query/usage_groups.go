package query

// UsageFileGroup is one file's worth of references from a
// group_by:"file" find_usages response. It lives here — beside
// SubGraph — because two layers speak this wire shape: the MCP
// renderer serializes it and the daemon's federation merge parses and
// re-emits it. One definition, so a field added for the renderer can
// never be silently stripped by the merge round trip.
type UsageFileGroup struct {
	File  string           `json:"file"`
	Count int              `json:"count"`
	Uses  []UsageGroupItem `json:"uses"`
}

// UsageGroupItem is one reference inside a UsageFileGroup — the line
// it sits on plus the enclosing symbol.
type UsageGroupItem struct {
	Line        int    `json:"line"`
	EdgeKind    string `json:"edge_kind"`
	Context     string `json:"context,omitempty"`
	ReturnUsage string `json:"return_usage,omitempty"`
	SymbolID    string `json:"symbol_id,omitempty"`
	SymbolName  string `json:"symbol_name,omitempty"`
}
