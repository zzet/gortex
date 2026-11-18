//go:build !gortex_edit_gate_seed

package mcp

import "context"

// Production builds cannot activate the deliberately slow regression seed.
func editGateACKSeed(context.Context, *Server) error { return nil }

func editGateCheckoutACKSeed(context.Context, *Server) error { return nil }
