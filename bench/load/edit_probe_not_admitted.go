package main

import "strings"

// editProbeNoMutationRefusal returns the code of a typed reply that refused
// an edit without changing any file, or "" for any other answer. The
// view_read_only texts are the checkout and byte admission refusals, which
// state that no files were changed; the view_building text is the session
// checkout gate. The texts are in edit_probe_refusal_text.go.
func editProbeNoMutationRefusal(attempt Attempt) string {
	message, ok := strings.CutPrefix(attempt.Error, toolErrorPrefix)
	if attempt.Outcome != "error" || attempt.Code != -32000 || !ok {
		return ""
	}
	if strings.HasPrefix(message, checkoutNotAdmittedPrefix) || strings.HasPrefix(message, byteNotAdmittedPrefix) {
		return "view_read_only"
	}
	if checkoutNotRouted(message) {
		return "view_building"
	}
	return ""
}

// editProbeRefusalCode is a refusal's typed code: the wire's error_code, or
// the code that leads a plain-text tool error ("<code>: <message>").
func editProbeRefusalCode(attempt Attempt) string {
	if code, _ := attempt.ErrorData["error_code"].(string); code != "" {
		return code
	}
	message, ok := strings.CutPrefix(attempt.Error, toolErrorPrefix)
	code, _, found := strings.Cut(message, ": ")
	if !ok || !found || code == "" || strings.ContainsAny(code, " \n") {
		return ""
	}
	return code
}

// editProbeNotAdmitted names the refusal that ended an edit's logical call
// when the product admitted none of its attempts, or returns "".
//
// LOAD re-sends an edit only after a busy or retryable refusal, which it
// already treats as not admitted. So the outcome is known when every earlier
// attempt was one of those and the call ended in a typed no-mutation reply,
// or in the backoff after such a refusal, when no further request was sent.
// A reply that never arrived, a tool deadline that abandoned running work, a
// busy reply whose delivery is unknown, and every other error stay unknown.
func editProbeNotAdmitted(attempts []Attempt) string {
	refusal := ""
	for i, attempt := range attempts {
		last := i == len(attempts)-1
		switch {
		case attempt.Outcome == "busy" && attempt.Code == -32002:
			refusal = "server_busy"
		case attempt.Outcome == "retryable":
			if refusal = editProbeRefusalCode(attempt); refusal == "" {
				return ""
			}
		case last && i > 0 && backoffEndedTerminal(attempt):
			// The logical deadline ended while waiting to re-send.
		case last:
			refusal = editProbeNoMutationRefusal(attempt)
		default:
			return ""
		}
	}
	return refusal
}
