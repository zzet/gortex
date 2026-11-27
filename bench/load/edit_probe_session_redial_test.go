package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// An edit probe's editor and prober sessions re-dial after a write timeout,
// and the report names each session that did. The edit call that absorbed a
// re-dial is marked in its attempt and counted in the edit's record, both
// carry its time, and each session row counts the records it marked.
func TestEditProbeReportsSessionRedials(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// Session 1 is the editor and 2 the edit's prober; both stall. Their
	// re-dials (3 and 4) are served after a measured handshake hold.
	socket, sessions, held := startRedialStub(t, func(connection int) stubStream {
		if connection <= 2 {
			return stubStall
		}
		return stubServeSlow
	})
	redialSession := map[string]int{"editor": 3, "prober": 4}
	corpus := editProbeCorpus()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, corpus.File), []byte("package p\nfunc Example() int { return 1 }\n"+corpus.EditAnchor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	editor, err := openMCP(ctx, socket, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = editor.Close() })
	phase := &Phase{Metrics: make(map[string]Metric)}
	p := &privateRun{Root: root, Socket: socket, Repos: []privateRepo{{Path: root, Corpus: corpus}}}
	run, probers, err := prepareEditProbe(ctx, runOptions{Seed: 1, EditCount: 1, Editors: 1, Deadline: time.Second}, p, nil, []*socketClient{editor}, phase)
	t.Cleanup(func() { closeClients(probers) })
	if run != nil {
		t.Cleanup(func() { _ = run.close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(probers) != 1 {
		t.Fatalf("probers: %d", len(probers))
	}
	state := run.states[0]
	status := request{Tool: "change", Operation: "status", Arguments: map[string]any{"operation": "status"}}
	for _, role := range []string{"editor", "prober"} {
		session := map[string]*socketClient{"editor": editor, "prober": probers[0]}[role]
		session.writeTimeout = 100 * time.Millisecond
		_, err := session.call(ctx, "large", redialLargeParams)
		requireWriteTimeout(t, err, role+"'s stalled write")
		for _, want := range []string{"re-dialed", "open"} {
			_, identity, attempts, err := run.performObserved(ctx, state, session, status, "status", "probe", 0)
			if err != nil {
				t.Fatalf("the edit probe's %s did not re-dial: %v", role, err)
			}
			if len(attempts) != 1 || attempts[0].Redialed != identity.Redialed || attempts[0].RedialNS != identity.Redial.Nanoseconds() {
				t.Fatalf("%s %s call: attempts %+v, identity re-dial mark %t, time %s", role, want, attempts, identity.Redialed, identity.Redial)
			}
			attempt, hold := attempts[0], held(redialSession[role])
			switch {
			case want == "open" && (attempt.Redialed || attempt.RedialNS != 0):
				t.Fatalf("%s call on an open stream carries redialed %t, redial_ns %d", role, attempt.Redialed, attempt.RedialNS)
			case want == "re-dialed" && !attempt.Redialed:
				t.Fatalf("%s re-dialed call is not marked redialed (redial_ns %d)", role, attempt.RedialNS)
			case want == "re-dialed" && (hold <= 0 || attempt.RedialNS < hold.Nanoseconds()):
				t.Fatalf("%s re-dialed call carries redial_ns %d, under the stub's %s handshake hold", role, attempt.RedialNS, hold)
			}
		}
	}
	state.mu.Lock()
	recordCalls, recordRedial := state.record.RedialedCalls, state.record.RedialNS
	state.mu.Unlock()
	if hold := held(3) + held(4); recordCalls != 2 || recordRedial < hold.Nanoseconds() {
		t.Fatalf("the edit record carries redialed_calls %d, redial_ns %d after two re-dialed calls holding %s", recordCalls, recordRedial, hold)
	}
	_ = run.finish() // The planned edit never ran; only the re-dial marks matter.
	want := []editProbeSessionRedials{{Role: "editor", Index: 0, Redials: 1, AffectedRecords: 1}, {Role: "prober", Index: 0, Redials: 1, AffectedRecords: 1}}
	if got := phase.EditProbe.SessionRedials; !reflect.DeepEqual(got, want) {
		t.Fatalf("session re-dials: %+v, want %+v", got, want)
	}
	if got := phase.EditProbe.Records; len(got) != 1 || got[0].RedialedCalls != 2 || got[0].RedialNS != recordRedial {
		t.Fatalf("records: %+v, want one with redialed_calls 2, redial_ns %d", got, recordRedial)
	}
	if got := sessions(); got != 4 {
		t.Fatalf("sessions accepted: %d, want 4", got)
	}
}
