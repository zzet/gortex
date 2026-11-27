package main

import "strings"

// The exact texts the edit refusal classifiers match, kept in one place so
// every classifier reads the same wording. Any other wording leaves an edit's
// outcome unknown.
const (
	// toolErrorPrefix leads a plain-text tool error (isError) as LOAD records
	// it: checkToolResult turns "<code>: <message>" into an RPC -32000 error.
	toolErrorPrefix = "RPC -32000: "

	// The checkout and byte admission refusals. Both state that no files were
	// changed.
	checkoutNotAdmittedPrefix = "view_read_only: checkout mutation was not admitted; no files were changed: "
	byteNotAdmittedPrefix     = "view_read_only: byte mutation admission refused; no files were changed: "

	// The checkout admission refusal's cause when the checkout's selected
	// route moved under the edit: the one cause a re-send waits out.
	checkoutRouteMovedCause = ": indexer: checkout mutation view is stale: the selected route moved"

	// The session checkout gate refuses a mutation while the checkout's route
	// is not ready, before any handler runs.
	checkoutNotRoutedPrefix = `view_building: checkout "`
	checkoutNotRoutedSuffix = `" is not fully routed yet`

	// busyBackoffEnded leads the synthetic terminal attempt of a logical call
	// whose deadline ended while it waited to re-send after a refusal.
	busyBackoffEnded = "busy backoff ended"
)

// checkoutNotRouted reports whether message is the session checkout gate's
// one-line not-routed refusal, without the tool error prefix.
func checkoutNotRouted(message string) bool {
	return strings.HasPrefix(message, checkoutNotRoutedPrefix) && strings.HasSuffix(message, checkoutNotRoutedSuffix) && !strings.Contains(message, "\n")
}

// backoffEndedTerminal reports whether attempt is the synthetic terminal LOAD
// appends when a logical call's deadline ends during a re-send backoff. No
// request was sent for it, so it has no code and no duration. A request that
// was sent and still had no reply at the deadline is not this terminal.
func backoffEndedTerminal(attempt Attempt) bool {
	return attempt.Code == 0 && attempt.DurationMS == 0 && strings.HasPrefix(attempt.Error, busyBackoffEnded)
}
