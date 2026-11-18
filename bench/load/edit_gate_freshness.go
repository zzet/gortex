package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	wire "github.com/gortexhq/gcx-go"
)

var editProbeTierLine = regexp.MustCompile(`^(file|relations|derived): (pending|pending_unknown|unknown|failed|fresh) \(([0-9]+) files\)$`)

type editProbeTierLabel struct {
	Scope string      `json:"scope"`
	State string      `json:"state"`
	Count json.Number `json:"count"`
}

// A typed, harness-local proof preserves every scope without allowing a JSON
// sibling to manufacture an extra freshness claim.
type editProbeTierLabels []editProbeTierLabel

const editProbeTierLabelsKey = "\x00load.tier.labels"

// The label is producer evidence, not disposable prose. Keep its exact scope,
// state and count while decoding the underlying compact/TOON payload normally.
func editProbeTierPrefix(text []byte) ([]byte, map[string]any, error) {
	var labels editProbeTierLabels
	seen := make(map[string]bool)
	for {
		first, rest, found := bytes.Cut(text, []byte("\n"))
		match := editProbeTierLine.FindSubmatch(first)
		if match == nil {
			break
		}
		if !found {
			return nil, nil, errors.New("freshness label has no payload boundary")
		}
		scope, state := string(match[1]), string(match[2])
		if seen[scope] {
			return nil, nil, errors.New("repeated freshness label for " + scope)
		}
		count, err := strconv.ParseUint(string(match[3]), 10, 63)
		if err != nil {
			return nil, nil, err
		}
		if state == "fresh" && count != 0 {
			return nil, nil, errors.New("freshness label claims fresh with pending files")
		}
		labels = append(labels, editProbeTierLabel{Scope: scope, State: state, Count: json.Number(strconv.FormatUint(count, 10))})
		seen[scope], text = true, rest
	}
	if len(labels) == 0 {
		return text, nil, nil
	}
	first := labels[0]
	return text, map[string]any{"graph_freshness": first.State, "freshness_scope": first.Scope, "pending_file_count": first.Count, editProbeTierLabelsKey: labels}, nil
}

func editProbeKeepTierFields(body, fields map[string]any) error {
	for key, value := range fields {
		if old, exists := body[key]; exists && fmt.Sprint(old) != fmt.Sprint(value) {
			return errors.New("payload contradicts its freshness label")
		}
		body[key] = value
	}
	return nil
}

// Only the existing GCX header decoder is used. Rows stay unsupported and can
// never supply payload/format credit from a TOON fallback or metadata alone.
func editProbeGCXTierFields(raw json.RawMessage, operation string) (map[string]any, error) {
	text, err := editOutputPayload(raw, false)
	if err != nil {
		return nil, err
	}
	header, err := wire.NewDecoder(bytes.NewReader(text)).Header()
	if err != nil {
		return nil, err
	}
	if canonical := editProbeCanonicalTool(operation); canonical == "" || header.Tool != canonical {
		return nil, errors.New("GCX header belongs to another operation")
	}
	state, scope, count := header.Meta["graph_freshness"], header.Meta["freshness_scope"], header.Meta["pending_file_count"]
	if state == "" && scope == "" && count == "" {
		return nil, nil
	}
	label := scope + ": " + state + " (" + count + " files)"
	_, fields, err := editProbeTierPrefix([]byte(label + "\n"))
	if err != nil || fields == nil {
		return nil, errors.New("incomplete or invalid GCX freshness metadata")
	}
	_, rows, _ := bytes.Cut(text, []byte("\n"))
	comment, _, _ := bytes.Cut(rows, []byte("\n"))
	if bytes.HasPrefix(comment, []byte("# ")) {
		observed := strings.TrimPrefix(string(comment), "# ")
		if editProbeTierLine.MatchString(observed) && observed != label {
			return nil, errors.New("GCX header and comment freshness disagree")
		}
	}
	return fields, nil
}

// Live daemon-clock observations remain separate from client and monotonic
// receipt windows. Missing stamps stay missing; equal clock cells are valid.
type editProbeLiveTiers struct {
	State     string            `json:"observation_state"`
	Reason    string            `json:"reason,omitempty"`
	ACK       string            `json:"ack_at,omitempty"`
	FileGraph string            `json:"file_graph_at,omitempty"`
	Relations string            `json:"relations_at,omitempty"`
	Derived   string            `json:"derived_at,omitempty"`
	Tiers     map[string]string `json:"tier_state,omitempty"`
}

func editProbeStatusTiers(body map[string]any) *editProbeLiveTiers {
	out := &editProbeLiveTiers{State: "observed"}
	previous := time.Time{}
	seen := false
	for _, field := range []struct {
		key    string
		target *string
	}{{"ack_at", &out.ACK}, {"file_graph_at", &out.FileGraph}, {"relations_at", &out.Relations}, {"derived_at", &out.Derived}} {
		raw, exists := body[field.key]
		if !exists {
			continue
		}
		seen = true
		value, ok := raw.(string)
		at, err := time.Parse(time.RFC3339Nano, value)
		if !ok || err != nil || at.IsZero() || !previous.IsZero() && at.Before(previous) {
			out.State, out.Reason = "invalid", "missing, malformed or reversed live tier stamp"
			return out
		}
		*field.target = value
		previous = at
	}
	if raw, exists := body["tier_state"]; exists {
		seen = true
		states, ok := raw.(map[string]any)
		if !ok {
			out.State, out.Reason = "invalid", "tier_state is not an object"
			return out
		}
		out.Tiers = map[string]string{}
		for _, tier := range []string{"file_graph", "relations", "derived"} {
			value, ok := states[tier].(string)
			if !ok || value != "fresh" && value != "pending" && value != "failed" && value != "partial" {
				out.State, out.Reason = "invalid", "unknown live tier state"
				return out
			}
			stamp := map[string]string{"file_graph": out.FileGraph, "relations": out.Relations, "derived": out.Derived}[tier]
			if value == "fresh" && stamp == "" {
				out.State, out.Reason = "invalid", "fresh live tier has no timestamp"
				return out
			}
			out.Tiers[tier] = value
		}
	}
	if !seen {
		return nil
	}
	return out
}
