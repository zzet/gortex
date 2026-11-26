package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEditConsumerGCXDeadCodeRequiresCompleteOwnedEvidence(t *testing.T) {
	const header = "GCX1 tool=analyze.dead_code fields=id,kind,name,path,line,reason"
	const unknown = "GCX1 tool=analyze.pending_unknown fields=id,kind,name,path,line,reason count=1\n"
	const pending = header + " count=0 complete=false freshness_scope=derived graph_freshness=pending pending_file_count=1\n# derived: pending (1 files)\n"
	c, plan := editConsumerTestFixture(t)
	target := fmt.Sprintf("%s\tfunction\thelper\t%s\t3\t\n", c.Symbol, c.File)
	var capped strings.Builder
	capped.WriteString(header + " count=1000\n")
	for i := range 1000 {
		fmt.Fprintf(&capped, "%s::symbol%d\tfunction\tsymbol%d\t%s\t3\t\n", c.File, i, i, c.File)
	}
	for _, tc := range []struct{ name, text, state string }{
		{"complete", header + " count=0\n", "fresh_membership_transition"},
		{"stale", header + " count=1\n" + target, "silent_stale"},
		{"known_pending", pending + unknown + target, "pending"},
		{"foreign_pending", pending + unknown + strings.ReplaceAll(target, c.File, "foreign.go"), "malformed"},
		{"missing_pending_section", pending, "malformed"},
		{"conflicting_comment", strings.Replace(pending, "(1 files)", "(2 files)", 1) + unknown + target, "malformed"},
		{"pending_comment_missing", strings.Replace(pending, "# derived: pending (1 files)\n", "", 1) + unknown + target, "malformed"},
		{"extra_section", pending + unknown + target + unknown + target, "malformed"},
		{"wrong_census", header + " count=1\n", "malformed"},
		{"at_producer_limit", capped.String(), "malformed"},
		{"json_substitution", `{"dead_code":[],"total":0,"truncated":false}`, "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			step := plan.Steps[0]
			step.ProbeMode = "json"
			sink := newEditProbeHeldSink(t)
			sink.unblock()
			s := startHeldEditProbeSpool(t, sink)
			s.step, s.record = step, newEditProbeRecord(c, step)
			now := time.Now()
			s.acknowledged = now
			r := &editProbeRuntime{origin: now, plan: plan, p: &privateRun{Repos: []privateRepo{{Corpus: c}}}, phase: &Phase{EditProbe: newEditProbeReport(plan, c, true)}}
			r.phase.EditProbe.ConsumerBaseline = &editConsumerBaseline{State: "OBSERVED_DEAD_BEFORE_EDIT", Symbol: c.Symbol}
			oracle := editConsumerOracles(c)[2]
			oracle.name += "_gcx"
			oracle.call.Arguments["output"] = map[string]any{"format": "gcx"}
			identity := wireCallIdentity{ID: 8, WriteStarted: now.Add(time.Millisecond), Finished: now.Add(2 * time.Millisecond)}
			if err := r.observeConsumer(s, oracle, outputWire(tc.text, nil), identity, nil); err != nil {
				t.Fatal(err)
			}
			row := s.record.Consumers[oracle.name]
			if row.State != tc.state || (row.SilentStale.Value == nil) != (tc.state == "malformed") {
				t.Fatalf("GCX dead-code observation=%+v, want state=%s", row, tc.state)
			}
			if tc.state == "silent_stale" && *row.SilentStale.Value != 1 {
				t.Fatalf("actual stale membership was not counted: %+v", row)
			}
		})
	}
}
