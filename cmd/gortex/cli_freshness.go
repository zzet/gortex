package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var cliRequireFresh bool

func applyCLIRequireFresh(args map[string]any) map[string]any {
	if cliRequireFresh {
		if args == nil {
			args = make(map[string]any)
		}
		args["require_fresh"] = true
	}
	return args
}

func checkCLIFreshness(cmd *cobra.Command, raw json.RawMessage, callErr error) error {
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if nested, ok := fields["freshness"].(map[string]any); ok {
		fields = nested
	}
	state, _ := fields["graph_freshness"].(string)
	scope, _ := fields["freshness_scope"].(string)
	if state == "" {
		stale, _ := fields["stale_files"].([]any)
		missing, _ := fields["missing_files"].([]any)
		count := len(stale) + len(missing)
		if count > 0 || fields["stale"] == true || fields["missing"] == true || fields["index_frozen"] == true {
			state, scope = "pending", "file"
			fields["pending_file_count"] = max(1, count)
		}
	}
	line := ""
	if state != "" && state != "fresh" {
		if scope == "" {
			scope = "relations"
		}
		count := fields["pending_file_count"]
		if count == nil {
			count = 0
		}
		line = fmt.Sprintf("%s: %s (%v files)", scope, state, count)
	} else {
		text := string(raw)
		if callErr != nil {
			var labelled bool
			text, labelled = strings.CutPrefix(callErr.Error(), "require_fresh: ")
			if !labelled {
				return callErr
			}
		} else if json.Valid(raw) {
			return nil // source strings inside JSON are not freshness metadata
		}
		text, _, _ = strings.Cut(text, "\n")
		for _, tier := range []string{"file", "relations", "derived"} {
			for _, status := range []string{"pending", "failed", "unknown"} {
				prefix := tier + ": " + status + " ("
				if tail, ok := strings.CutPrefix(text, prefix); ok {
					if count, _, closed := strings.Cut(tail, " files)"); closed {
						line = prefix + count + " files)"
					}
				}
			}
		}
	}
	if line == "" {
		return callErr
	}
	fmt.Fprintln(cmd.ErrOrStderr(), line)
	if cliRequireFresh {
		cmd.SilenceErrors = true
		return &exitCodeError{code: 4}
	}
	return callErr
}
