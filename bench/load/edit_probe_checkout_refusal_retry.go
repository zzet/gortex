package main

import (
	"context"
	"encoding/json"
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
// admission, so re-sending the same edit cannot apply it twice. The texts are
// in edit_probe_refusal_text.go and match the product's messages exactly; any
// other wording, cause or second content item stays terminal.
func editProbeTransientCheckoutRefusal(message string) string {
	if strings.Contains(message, "\n") {
		return ""
	}
	if checkoutNotRouted(message) {
		return "view_building"
	}
	if strings.HasPrefix(message, checkoutNotAdmittedPrefix) && strings.Contains(message, checkoutRouteMovedCause) {
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

// noteResentReplay flags a re-sent edit whose acknowledgement says replayed:
// the product's ledger answered its mutation id from an earlier attempt that
// committed, although every earlier reply refused the edit. The flag changes
// no outcome. The caller does not hold s.mu.
func (s *editProbeState) noteResentReplay(ack json.RawMessage, output, tool string) {
	body, err := decodeEditProbeOutput(ack, output, tool)
	if replayed, _ := body["replayed"].(bool); err != nil || !replayed {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record.ResentEditReplayed = true
}
