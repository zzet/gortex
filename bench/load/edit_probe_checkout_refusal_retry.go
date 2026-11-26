package main

import (
	"context"
	"errors"
	"strings"
	"time"
)

// editProbeTransientCheckoutRefusal returns the code of a typed checkout
// refusal that clears on its own, or "" for any other message.
//
// view_building: the session checkout gate refuses a mutation while the
// checkout's route is not ready; the product's guidance is to retry, with no
// reconnect. view_read_only on a moved route: the product refused admission,
// changed no files, and asks for a retry on a fresh view. Both refuse before
// admission, so re-sending the same edit cannot apply it twice. The texts
// match the product's messages exactly; any other wording stays terminal.
func editProbeTransientCheckoutRefusal(message string) string {
	if strings.Contains(message, "\n") {
		return ""
	}
	if strings.HasPrefix(message, `view_building: checkout "`) && strings.HasSuffix(message, `" is not fully routed yet`) {
		return "view_building"
	}
	if strings.HasPrefix(message, "view_read_only: checkout mutation was not admitted; no files were changed: ") &&
		strings.Contains(message, ": indexer: checkout mutation view is stale: the selected route moved") {
		return "view_read_only"
	}
	return ""
}

// The product waits for the checkout's route before it refuses, so a typed
// checkout refusal can take most of the product's own tool deadline. A
// re-send is made only when the time left before the call's deadline exceeds
// the refused attempt's own time plus the longest backoff wait. Otherwise the
// re-send would most likely still be in flight at the deadline, turning the
// product's "no files were changed" into an unknown outcome.
func editProbeResendFits(ctx context.Context, start, returned time.Time, backoff time.Duration) bool {
	deadline, bounded := ctx.Deadline()
	return !bounded || deadline.Sub(returned) > returned.Sub(start)+editProbeBackoffCeiling(backoff)
}

// editProbeResendDeclined ends a logical edit call on a transient checkout
// refusal that was not re-sent because no re-send could be answered before
// the deadline. Its text is the product's refusal.
type editProbeResendDeclined struct {
	code string
	err  error
}

func (e *editProbeResendDeclined) Error() string { return e.err.Error() }
func (e *editProbeResendDeclined) Unwrap() error { return e.err }

// editProbeDeclinedResend returns the refusal code when err ended an edit on a
// declined re-send, or "". The record then ends on that refusal as a product
// deadline, with the code as its reason.
func editProbeDeclinedResend(err error) string {
	var declined *editProbeResendDeclined
	if errors.As(err, &declined) {
		return declined.code
	}
	return ""
}
