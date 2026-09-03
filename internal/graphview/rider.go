package graphview

import "strings"

// ViewRider is the block every view-aware response carries: which view the
// caller asked for, which one actually answered, and — when those differ — why.
// Without it a fallback is invisible, and a client cannot tell an answer about
// the branch it asked for from an answer about whatever the server had ready.
type ViewRider struct {
	// RequestedView is the selector the caller sent, in Selector.String form.
	RequestedView string `json:"requested_view,omitempty"`
	// ActualView is what served the request: a selector string, or the
	// fingerprint of the view identity when the server pinned one.
	ActualView string `json:"actual_view,omitempty"`
	// GraphID and CheckoutID name the concrete graph and checkout behind
	// ActualView.
	GraphID    string `json:"graph_id,omitempty"`
	CheckoutID string `json:"checkout_id,omitempty"`
	// ViewFingerprint is the identity of the content that answered. It is the
	// authority half of every gortex-view:// file URI in the same response.
	ViewFingerprint string `json:"view_fingerprint,omitempty"`
	// RequestedRef is the ref or object id the caller's selector named,
	// verbatim. ResolvedRef, ResolvedCommit and ResolvedTree are what it
	// resolved to when the request was served — the ref is empty for a commit
	// selector, which names no ref.
	RequestedRef   string `json:"requested_ref,omitempty"`
	ResolvedRef    string `json:"resolved_ref,omitempty"`
	ResolvedCommit string `json:"resolved_commit,omitempty"`
	ResolvedTree   string `json:"resolved_tree,omitempty"`
	// RequestedState and ActualState are the readiness the caller required and
	// the readiness it got.
	RequestedState string `json:"requested_state,omitempty"`
	ActualState    string `json:"actual_state,omitempty"`
	// Exact reports whether ActualView is the view that was requested.
	Exact bool `json:"exact"`
	// FallbackReason explains an inexact answer. It is set exactly when Exact
	// is false.
	FallbackReason string `json:"fallback_reason,omitempty"`
	// BuildToken identifies an in-progress build the caller can poll.
	BuildToken string `json:"build_token,omitempty"`
	// RetryAfter is a hint in whole seconds; 0 means no hint.
	RetryAfter int64 `json:"retry_after,omitempty"`
}

// NewViewRider starts a rider for a request, exact until something says
// otherwise.
func NewViewRider(requested Selector) *ViewRider {
	return &ViewRider{RequestedView: requested.String(), Exact: true}
}

// MarkExact records that the requested view is the one that answered, clearing
// any fallback reason left from an earlier attempt.
func (r *ViewRider) MarkExact(actualView string) {
	r.ActualView = actualView
	r.Exact = true
	r.FallbackReason = ""
}

// MarkFallback records that something other than the requested view answered.
// A reason is mandatory — an unexplained fallback is indistinguishable from a
// bug on the client side — so a blank one is rejected with
// CodeInvalidViewSelector and the rider is left untouched.
func (r *ViewRider) MarkFallback(actualView, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return NewViewError(CodeInvalidViewSelector, "a fallback rider requires a reason")
	}
	r.ActualView = actualView
	r.Exact = false
	r.FallbackReason = reason
	return nil
}
