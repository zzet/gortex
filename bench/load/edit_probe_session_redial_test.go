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
// and the report names each session that did.
func TestEditProbeReportsSessionRedials(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// Session 1 is the editor and 2 the edit's prober; both stall. Their
	// re-dials (3 and 4) are served.
	socket, sessions := startRedialStub(t, func(connection int) stubStream {
		if connection <= 2 {
			return stubStall
		}
		return stubServe
	})
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
	for role, session := range map[string]*socketClient{"editor": editor, "prober": probers[0]} {
		session.writeTimeout = 100 * time.Millisecond
		_, err := session.call(ctx, "large", redialLargeParams)
		requireWriteTimeout(t, err, role+"'s stalled write")
		if _, err := session.call(ctx, "after", nil); err != nil {
			t.Fatalf("the edit probe's %s did not re-dial: %v", role, err)
		}
	}
	_ = run.finish() // The planned edit never ran; only the session rows matter.
	want := []editProbeSessionRedials{{Role: "editor", Index: 0, Redials: 1}, {Role: "prober", Index: 0, Redials: 1}}
	if got := phase.EditProbe.SessionRedials; !reflect.DeepEqual(got, want) {
		t.Fatalf("session re-dials: %+v, want %+v", got, want)
	}
	if got := sessions(); got != 4 {
		t.Fatalf("sessions accepted: %d, want 4", got)
	}
}
