package graphview

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNewViewRiderStartsExact(t *testing.T) {
	sel, err := ParseSelector("git_ref", "", "", "refs/heads/main")
	if err != nil {
		t.Fatalf("ParseSelector() = %v", err)
	}
	r := NewViewRider(sel)
	if r.RequestedView != "git_ref:refs/heads/main" {
		t.Errorf("RequestedView = %q", r.RequestedView)
	}
	if !r.Exact {
		t.Error("a fresh rider is not Exact")
	}
	if r.FallbackReason != "" {
		t.Errorf("FallbackReason = %q, want empty", r.FallbackReason)
	}
}

func TestMarkFallbackRequiresAReason(t *testing.T) {
	r := NewViewRider(Selector{Kind: SelectorAuto})
	r.MarkExact("base:graph-1")

	for _, reason := range []string{"", "   ", "\t\n"} {
		err := r.MarkFallback("base:graph-2", reason)
		if err == nil {
			t.Fatalf("MarkFallback(%q) = nil, want an error", reason)
		}
		if got := CodeOf(err); got != CodeInvalidViewSelector {
			t.Errorf("CodeOf() = %q, want %q", got, CodeInvalidViewSelector)
		}
		if !errors.Is(err, ErrInvalidViewSelector) {
			t.Error("error does not match the invalid_view_selector sentinel")
		}
		if !r.Exact || r.ActualView != "base:graph-1" || r.FallbackReason != "" {
			t.Fatalf("a rejected fallback modified the rider: %+v", r)
		}
	}
}

func TestMarkFallbackAndMarkExact(t *testing.T) {
	r := NewViewRider(Selector{Kind: SelectorGitRef, Value: "refs/heads/main"})
	if err := r.MarkFallback("base:graph-1", "ref is not indexed yet"); err != nil {
		t.Fatalf("MarkFallback() = %v", err)
	}
	if r.Exact {
		t.Error("Exact stayed true after a fallback")
	}
	if r.ActualView != "base:graph-1" {
		t.Errorf("ActualView = %q", r.ActualView)
	}
	if r.FallbackReason != "ref is not indexed yet" {
		t.Errorf("FallbackReason = %q", r.FallbackReason)
	}

	// A later exact answer clears the stale reason.
	r.MarkExact("git_ref:refs/heads/main")
	if !r.Exact {
		t.Error("MarkExact left Exact false")
	}
	if r.ActualView != "git_ref:refs/heads/main" {
		t.Errorf("ActualView = %q", r.ActualView)
	}
	if r.FallbackReason != "" {
		t.Errorf("FallbackReason = %q, want empty", r.FallbackReason)
	}
}

func TestViewRiderJSONShape(t *testing.T) {
	r := &ViewRider{
		RequestedView:  "git_ref:refs/heads/main",
		ActualView:     "9cf01efe",
		GraphID:        "graph-1",
		CheckoutID:     "wt-7",
		RequestedState: "complete",
		ActualState:    "building",
		Exact:          false,
		FallbackReason: "view is still building",
		BuildToken:     "build-42",
		RetryAfter:     3,
	}
	blob, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"requested_view":"git_ref:refs/heads/main","actual_view":"9cf01efe",` +
		`"graph_id":"graph-1","checkout_id":"wt-7","requested_state":"complete",` +
		`"actual_state":"building","exact":false,"fallback_reason":"view is still building",` +
		`"build_token":"build-42","retry_after":3}`
	if string(blob) != want {
		t.Fatalf("json = %s, want %s", blob, want)
	}

	// An exact rider carries only what it needs; "exact" is never omitted,
	// because a missing flag would read as an inexact answer.
	blob, err = json.Marshal(&ViewRider{RequestedView: "auto", Exact: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const wantExact = `{"requested_view":"auto","exact":true}`
	if string(blob) != wantExact {
		t.Errorf("json = %s, want %s", blob, wantExact)
	}
}
