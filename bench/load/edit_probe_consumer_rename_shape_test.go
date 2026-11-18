package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEditConsumerRenameUnassessableSiteNeverClaimsStale(t *testing.T) {
	for _, variant := range []string{"multiline_actual_site", "sparse_site"} {
		t.Run(variant, func(t *testing.T) {
			c := editProbeCorpus()
			c.Symbol, c.Query = c.File+"::helper", "helper"
			c.EditAnchor = "func helper() int { return 1 }"
			// A valid offered template, not a mutation of a retained artifact.
			// rename_symbol returns individual source lines, so the call row
			// has no function/marker identity for this multiline definition.
			c.EditTemplate = "\nfunc {{marker}}() int {\n    return helper()\n}\n"
			c.ProposedSignature = "func helper(probe int) int"
			plan, err := planEditProbe(c, runOptions{Seed: 1, EditCount: 1, Editors: 1, EditConsumerProbes: true})
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			site := map[string]any{"file": c.File, "line": 5, "old_text": "    return helper()", "new_text": "    return helperRenamed()", "confidence": "high", "reason": "call reference"}
			if variant == "sparse_site" {
				delete(site, "old_text")
				delete(site, "new_text")
			}
			body := map[string]any{"status": "would_apply", "dry_run": true, "old_name": "helper", "new_name": "helperRenamed", "total_edits": 2, "files_affected": 1, "edits": []any{
				map[string]any{"file": c.File, "line": 2, "old_text": c.EditAnchor, "new_text": "func helperRenamed() int { return 1 }", "confidence": "high", "reason": "definition"},
				site,
			}}
			payload, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			s := startHeldEditProbeSpool(t, sink)
			s.step, s.record = step, newEditProbeRecord(c, step)
			for name, other := range s.record.Consumers {
				if name != "consumer_rename" {
					other.PostACKAttempts, other.Assessed = 1, 1
					other.State, other.SilentStale = "subject_observed", measured(0, "answers")
					s.record.Consumers[name] = other
				}
			}
			origin := time.Now()
			s.started, s.acknowledged = origin, origin.Add(time.Millisecond)
			r := &editProbeRuntime{origin: origin, plan: plan, p: &privateRun{Repos: []privateRepo{{Corpus: c}}}, phase: &Phase{EditProbe: newEditProbeReport(plan, c, true)}}
			identity := wireCallIdentity{WriteStarted: origin.Add(2 * time.Millisecond), Finished: origin.Add(3 * time.Millisecond)}
			if err := r.observeConsumer(s, editConsumerOracles(c)[0], outputWire(string(payload), nil), identity, nil); err != nil {
				t.Fatal(err)
			}
			row := s.record.Consumers["consumer_rename"]
			if row.State != "unassessed" || row.SilentStale.Value != nil || row.PostACKAttempts != 1 || row.Unassessed != 1 || row.Assessed != 0 {
				t.Fatalf("unidentified rename row fabricated stale/clean coverage: %+v", row)
			}
			if !editConsumersFinished(s.record.Consumers) || !editConsumersFailure(s.record.Consumers) {
				t.Fatal("unassessed caller gained completed consumer coverage")
			}
		})
	}
}
