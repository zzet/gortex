// Package mql re-exports the tree-sitter-mql5 grammar. The C parser
// lives in the github.com/davalillo/tree-sitter-mql5 module (a fork of
// mskelton/tree-sitter-mql5 with a Go binding and corpus-driven MQL5
// grammar extensions); this file is just the thin shim that bridges the
// upstream binding into gortex's *tsitter.Language type.
//
// One grammar serves .mq4, .mq5 and .mqh: modern MQL4 (build 600+)
// shares MQL5 syntax, and pre-600 legacy MQL4 is a C subset.
package mql

import (
	tree_sitter_mql5 "github.com/davalillo/tree-sitter-mql5/bindings/go"
	"github.com/zzet/gortex/internal/parser/tsitter"
)

// GetLanguage returns the compiled MQL language.
func GetLanguage() *tsitter.Language {
	return tsitter.NewLanguage(tree_sitter_mql5.Language())
}
