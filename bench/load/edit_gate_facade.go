package main

import (
	"encoding/json"
	"errors"
	"strings"
)

type editProbeFacadeWire struct{ operation string }

const editProbeFacadeWireKey = "\x00load.facade.wire"

// Surface identity is proof from the actual MCP envelope, independent from
// its JSON payload. A GCX/TOON decoder or structured sibling cannot supply it.
func verifyEditProbeFacade(raw json.RawMessage, operation string) error {
	canonical := editProbeCanonicalTool(operation)

	facade, op, ok := strings.Cut(operation, ".")
	if !ok || canonical == "" {
		return errors.New("facade dispatch proof unavailable for this operation")
	}
	var wire struct {
		Meta struct {
			Identity struct {
				Surface   string `json:"surface_version"`
				Facade    string `json:"facade"`
				Operation string `json:"operation"`
				Canonical string `json:"canonical_tool"`
			} `json:"gortex_facade"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	identity := wire.Meta.Identity
	if identity.Surface != "facade-v1" || identity.Facade != facade || identity.Operation != op || identity.Canonical != canonical {
		return errors.New("missing or mismatched facade-v1 dispatch identity")
	}
	return nil
}

func editProbeCanonicalTool(operation string) string {
	return map[string]string{
		"read.file": "read_file", "read.source": "get_symbol_source", "read.summary": "get_file_summary", "read.editing_context": "get_editing_context",
		"search.symbols": "search_symbols", "search.text": "search_text", "relations.callers": "get_callers", "trace.call_chain": "get_call_chain",
	}[operation]
}
