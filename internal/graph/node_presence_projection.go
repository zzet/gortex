package graph

import "context"

// NodePresenceByIDsReader answers existence for requested identities in the
// exact selected reader. Every stored kind, including empty and unknown kinds,
// counts as present. Errors and cancellation must return no usable partial set.
// Physical implementations must not be forwarded through a composed or scoped
// reader unless that reader applies its own visibility and ownership rules.
type NodePresenceByIDsReader interface {
	GetNodePresenceByIDsContext(context.Context, []string) (map[string]struct{}, error)
}
