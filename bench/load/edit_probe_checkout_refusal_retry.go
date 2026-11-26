package main

import "strings"

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
