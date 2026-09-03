package daemon

import (
	"context"
	"encoding/json"
	"testing"
)

// coverageController answers the coverage kind and records what it was asked.
type coverageController struct {
	Controller
	asked  string
	result FileCoverageResult
}

func (c *coverageController) FileCoverage(_ context.Context, p FileCoverageParams) (FileCoverageResult, error) {
	c.asked = p.Path
	return c.result, nil
}

func controlFileCoverage(t *testing.T, ctrl Controller, params any) ControlResponse {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	s := New(t.TempDir()+"/sock", "test", nil)
	s.Controller = ctrl
	return s.handleControl(context.Background(), nil, ControlRequest{
		Kind:   ControlFileCoverage,
		Params: raw,
	})
}

// TestControlFileCoverageCarriesTheViewBlock pins the dispatch: the path
// reaches the controller and the view block survives the round trip, which is
// the only thing that tells a caller whether the answer was the path's own.
func TestControlFileCoverageCarriesTheViewBlock(t *testing.T) {
	ctrl := &coverageController{result: FileCoverageResult{
		Covered: true,
		Symbols: 4,
		View: &ProbeView{
			Kind:           ProbeViewBase,
			CheckoutID:     "co-1",
			Exact:          false,
			FallbackReason: "availability_grace",
		},
	}}
	resp := controlFileCoverage(t, ctrl, FileCoverageParams{Path: "/wt/internal/x.go"})
	if !resp.OK {
		t.Fatalf("file_coverage failed [%s]: %s", resp.ErrorCode, resp.ErrorMsg)
	}
	if ctrl.asked != "/wt/internal/x.go" {
		t.Fatalf("controller was asked about %q, not the probed path", ctrl.asked)
	}
	var out FileCoverageResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !out.Covered || out.Symbols != 4 {
		t.Fatalf("coverage = %+v, want covered with 4 symbols", out)
	}
	if out.View == nil || out.View.Exact || out.View.FallbackReason != "availability_grace" {
		t.Fatalf("view = %+v, want a fallback answer that says so", out.View)
	}
}

// TestControlFileCoverageIsRefusedByAControllerThatCannotAnswer pins the
// degradation: a daemon whose controller cannot resolve a path to a view says
// so instead of guessing, and the caller reads it the way it reads a daemon
// that is not running — no signal, so the native tool proceeds.
func TestControlFileCoverageIsRefusedByAControllerThatCannotAnswer(t *testing.T) {
	resp := controlFileCoverage(t, statusOnlyController{}, FileCoverageParams{Path: "/wt/x.go"})
	if resp.OK {
		t.Fatal("a controller with no coverage support answered the coverage kind")
	}
	if resp.ErrorMsg == "" {
		t.Fatal("the refusal carries no reason")
	}
}

// TestFileCoverageResultOmitsTheViewBlockWhenAbsent pins the wire shape a
// controller that resolved nothing produces: no view key at all, so a consumer
// cannot mistake a zero-valued block for a real "exact: false".
func TestFileCoverageResultOmitsTheViewBlockWhenAbsent(t *testing.T) {
	encoded, err := json.Marshal(FileCoverageResult{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(encoded), `{"covered":false,"symbols":0}`; got != want {
		t.Fatalf("encoded = %s, want %s", got, want)
	}
}

// TestSearchSymbolsParamsOmitThePathWhenUnset pins the compatibility half of
// the params: a client that predates routed views sends the same bytes it
// always did.
func TestSearchSymbolsParamsOmitThePathWhenUnset(t *testing.T) {
	encoded, err := json.Marshal(SearchSymbolsParams{Query: "handleFoo", Limit: 10})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(encoded), `{"query":"handleFoo","limit":10}`; got != want {
		t.Fatalf("encoded = %s, want %s", got, want)
	}
}
