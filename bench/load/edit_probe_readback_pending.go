package main

import "strconv"

// A linked checkout's full ACK returns before the checkout's queued
// publication (reindex_pending, graph_status pending). Until the checkout's
// route republishes, a cwd-bound graph read on the same session is answered
// from the labelled read-only base fallback: the reply's freshness rider names
// the checkout's own view as requested, another view as actual, exact false
// and a typed fallback reason. Such an immediate readback is pending, never
// incorrect: LOAD re-reads it on the same session until the rider is exact for
// the requested view, then scores that answer with the kind's oracle. A wrong
// exact answer, a fallback for any other view or reason, a primary fallback
// and any wrong disk read stay incorrect.

// Graph-served immediate readback kinds. file and source_file_after_move are
// served from disk and must be correct at once.
var editProbePendingReadbackKinds = map[string]bool{"source": true, "old_source_absent": true, "summary": true, "search_visible": true, "text": true}

// Fallback reasons the product documents as a view that is still being
// (re)published; route_moved and any other reason stay incorrect.
var editProbePendingFallbackReasons = map[string]bool{"view_building": true, "discovery_pending": true}

// One pending kind of one record. Times are client milliseconds from edit
// send; first_exact_ms stays unavailable when no exact answer arrived before
// the record ended.
type editProbeReadbackPending struct {
	PendingReason     string `json:"pending_reason"`
	FirstPending      Metric `json:"first_pending_ms"`
	Rereads           int    `json:"rereads"`
	FirstExact        Metric `json:"first_exact_ms"`
	FirstExactCorrect *bool  `json:"first_exact_correct,omitempty"`
}

// The edit's own ACK documents its pending publication on the run's admitted
// linked checkout: applied, committed, this step's mutation, publication
// pending, the checkout named by the ACK's view, and not a replay. This is the
// ACK-side half of linkedFullAcknowledgementMatches; no receipt is read
// before the immediate readbacks.
func editProbeLinkedPendingACK(ack map[string]any, mutationID, checkoutID string) bool {
	if checkoutID == "" || mutationID == "" || ack == nil {
		return false
	}
	view, _ := ack["freshness"].(map[string]any)
	if ack["status"] != "applied" || ack["disk_status"] != "committed" || ack["mutation_id"] != mutationID ||
		ack["reindex_pending"] != true || ack["graph_status"] != "pending" || view["checkout_id"] != checkoutID {
		return false
	}
	replayed, present := ack["replayed"]
	return !present || replayed == false
}

// The rider labels the reply a substitution for the checkout's own requested
// view, for a typed pending reason.
func editProbeRiderPending(d *editProbeReadbackDisposition, checkoutID string) bool {
	requested := "worktree:" + checkoutID
	return d != nil && checkoutID != "" && d.RequestedView == requested && d.Exact != nil && !*d.Exact &&
		d.ActualView != requested && editProbePendingFallbackReasons[d.FallbackReason]
}

// The rider says the reply is exact for the checkout's own requested view.
func editProbeRiderExact(d *editProbeReadbackDisposition, checkoutID string) bool {
	requested := "worktree:" + checkoutID
	return d != nil && checkoutID != "" && d.RequestedView == requested && d.Exact != nil && *d.Exact && d.ActualView == requested
}

// An immediate readback is pending only when every condition holds: the
// edit's route is the run's own linked checkout, its ACK documents the
// pending publication, the kind is graph-served, the rider is pending for the
// checkout's own view, and the kind's oracle did not accept the reply.
func editProbeReadbackIsPending(back editProbeReadback, checkoutID string, pendingACK bool) bool {
	return checkoutID != "" && pendingACK && editProbePendingReadbackKinds[back.Kind] &&
		editProbeRiderPending(back.Disposition, checkoutID) && !back.Correct
}

// Why a re-read ended the pending contract without an exact answer: the
// rider's fields, then the reply's disposition kind and transport or decode
// error, so a failed re-read is not reported as a missing rider alone.
func editProbeRiderDeparture(back editProbeReadback) string {
	d := back.Disposition
	out := "no freshness rider"
	if d != nil && (d.RequestedView != "" || d.ActualView != "" || d.Exact != nil || d.FallbackReason != "") {
		exact := "absent"
		if d.Exact != nil {
			exact = strconv.FormatBool(*d.Exact)
		}
		out = "requested_view " + d.RequestedView + ", actual_view " + d.ActualView + ", exact " + exact + ", fallback_reason " + d.FallbackReason
	}
	if d != nil && d.Kind != "" {
		out += "; disposition " + d.Kind
	}
	if back.Error != "" {
		out += ": " + back.Error
	}
	return out
}
