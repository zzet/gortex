package lsp

import (
	"testing"
	"time"
)

func publishForTest(p *Provider, path string, diags []Diagnostic) {
	p.docMu.Lock()
	p.lastDiag[path] = diags
	p.docMu.Unlock()
	p.fanoutDiagnostics(path, diags)
}

func TestWaitForSettledDiagnosticsOutlastsAnEmptyFirstPublish(t *testing.T) {
	const path = "/w/a.vue"
	p := &Provider{
		diagWaiters: map[string][]chan []Diagnostic{},
		lastDiag:    map[string][]Diagnostic{},
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		publishForTest(p, path, []Diagnostic{})
		time.Sleep(60 * time.Millisecond)
		publishForTest(p, path, []Diagnostic{{Message: "real"}})
	}()

	start := time.Now()
	p.WaitForSettledDiagnostics(path, 5*time.Second, 300*time.Millisecond)

	if got, _ := p.LastDiagnostics(path); len(got) != 1 {
		t.Fatalf("settled on %v, want the second publish", got)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %v: a quiet server must end the wait, not the timeout", took)
	}
}

func TestWaitForSettledDiagnosticsGivesUpAtTimeout(t *testing.T) {
	p := &Provider{
		diagWaiters: map[string][]chan []Diagnostic{},
		lastDiag:    map[string][]Diagnostic{},
	}
	start := time.Now()
	p.WaitForSettledDiagnostics("/w/a.vue", 50*time.Millisecond, 10*time.Second)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %v, want it bounded by the timeout", took)
	}
	if _, ok := p.LastDiagnostics("/w/a.vue"); ok {
		t.Fatal("nothing was published")
	}
}
